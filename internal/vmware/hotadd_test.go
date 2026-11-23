package vmware

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/types"
)

// stubHotAddOS replaces the agent-side parts of hot-add (BIOS UUID, block
// devices, lock) for tests: the "device" of an attached disk is dev.
func stubHotAddOS(t *testing.T, uuid string, dev string, devErr error) *int {
	t.Helper()
	u, b, w, rl, l := ownVMUUIDs, blockDevices, waitForDisk, releaseDisk, lockHotAdd
	t.Cleanup(func() { ownVMUUIDs, blockDevices, waitForDisk, releaseDisk, lockHotAdd = u, b, w, rl, l })
	released := 0
	ownVMUUIDs = func() ([]string, error) { return []string{uuid}, nil }
	blockDevices = func() (map[string]uint64, error) { return map[string]uint64{}, nil }
	waitForDisk = func(context.Context, map[string]uint64, uint64) (string, error) { return dev, devErr }
	releaseDisk = func(string, func(string, ...any)) { released++ }
	lockHotAdd = func() (func(), error) { return func() {}, nil }
	return &released
}

// borrowedDisks lists the proxy's disks that hot-add attached.
func borrowedDisks(t *testing.T, ctx context.Context, h *hotAdd) []string {
	t.Helper()
	devs, err := h.proxy.Device(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range devs.SelectByType((*types.VirtualDisk)(nil)) {
		if f, ok := h.borrowed(d.(*types.VirtualDisk)); ok {
			out = append(out, f)
		}
	}
	return out
}

// TestHotAddSimulator attaches a disk of one simulated VM to another (the
// "agent's VM"), reads it, detaches it without deleting it, and detaches
// what an interrupted backup left attached.
func TestHotAddSimulator(t *testing.T) {
	ctx := context.Background()
	model := simulator.ESX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	s := model.Service.NewServer()
	defer s.Close()
	pw, _ := s.URL.User.Password()
	c, err := Connect(ctx, Conn{Host: s.URL.Host, User: s.URL.User.Username(), Password: pw, HotAdd: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	vms, err := c.VMs(ctx)
	if err != nil || len(vms) < 2 {
		t.Fatalf("VMs: %v %v", vms, err)
	}
	proxy, src := vms[0], vms[1]
	bios := object.NewVirtualMachine(c.vim, proxy.Ref).UUID(ctx)
	if bios == "" {
		t.Fatal("the simulated VM has no BIOS UUID")
	}
	data := bytes.Repeat([]byte("disk"), 1024)
	dev := filepath.Join(t.TempDir(), "sdx")
	if err := os.WriteFile(dev, data, 0o600); err != nil {
		t.Fatal(err)
	}
	released := stubHotAddOS(t, bios, dev, nil)
	logs := []string{}
	log := func(msg string, _ ...any) { logs = append(logs, msg) }

	h, err := c.hotAddProxy(ctx, log)
	if err != nil || h == nil {
		t.Fatalf("proxy not found: %v", err)
	}
	if h.name != proxy.Name {
		t.Errorf("proxy %q, want %q", h.name, proxy.Name)
	}
	if b := borrowedDisks(t, ctx, h); len(b) != 0 {
		t.Fatalf("the proxy's own disk counts as borrowed: %v", b)
	}
	file := src.Disks[0].File
	ra, release, err := h.attach(ctx, file, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if b := borrowedDisks(t, ctx, h); len(b) != 1 || b[0] != file {
		t.Fatalf("attached disks %v, want %s", b, file)
	}
	got := make([]byte, len(data))
	if _, err := ra.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Error("read through the attached disk differs")
	}
	release()
	if b := borrowedDisks(t, ctx, h); len(b) != 0 || *released != 1 {
		t.Fatalf("after release: attached %v, released %d", b, *released)
	}
	// The source VM still has its disk (detaching never deletes the file).
	vms, _ = c.VMs(ctx)
	for _, v := range vms {
		if v.Name == src.Name && (len(v.Disks) == 0 || v.Disks[0].File != file) {
			t.Fatalf("source disks after detaching: %+v", v.Disks)
		}
	}

	// M-12 (0.59): while one backup reads the disk, another backup of the
	// same agent starting leaves it attached.
	_, release2, err := h.attach(ctx, file, uint64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.hotAddProxy(ctx, log); err != nil {
		t.Fatal(err)
	}
	if b := borrowedDisks(t, ctx, h); len(b) != 1 || b[0] != file {
		t.Fatalf("a disk another backup is reading was detached: %v", b)
	}
	release2()
	release2() // a second call changes nothing
	if b := borrowedDisks(t, ctx, h); len(b) != 0 || isInUse(file) {
		t.Fatalf("after release: attached %v, in use %v", b, isInUse(file))
	}

	// An interrupted backup (agent stopped) left a disk attached: after the
	// restart nothing is in use, and the next backup detaches it.
	if _, _, err := h.attach(ctx, file, uint64(len(data))); err != nil {
		t.Fatal(err)
	}
	inUse.Lock()
	clear(inUse.files) // the agent restarted
	inUse.Unlock()
	h2, err := c.hotAddProxy(ctx, log)
	if err != nil {
		t.Fatal(err)
	}
	if b := borrowedDisks(t, ctx, h2); len(b) != 0 {
		t.Fatalf("leftover not detached: %v", b)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "left attached by an earlier backup") {
		t.Errorf("logs %q", logs)
	}

	// The disk does not show up in the agent's VM: detached again, error.
	stubHotAddOS(t, bios, "", errors.New("did not appear"))
	if _, _, err := h2.attach(ctx, file, uint64(len(data))); err == nil {
		t.Fatal("attach without a device succeeded")
	}
	if b := borrowedDisks(t, ctx, h2); len(b) != 0 {
		t.Fatalf("a disk that did not appear stays attached: %v", b)
	}
}

// Hot-add off, or the agent's machine not a VM of the host: no proxy.
func TestHotAddUnavailable(t *testing.T) {
	ctx := context.Background()
	model := simulator.ESX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	s := model.Service.NewServer()
	defer s.Close()
	pw, _ := s.URL.User.Password()
	c, err := Connect(ctx, Conn{Host: s.URL.Host, User: s.URL.User.Username(), Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	if h, err := c.hotAddProxy(ctx, func(string, ...any) {}); h != nil || err != nil {
		t.Fatalf("hot-add off: %v %v", h, err)
	}
	c.conn.HotAdd = true
	stubHotAddOS(t, "00000000-0000-0000-0000-000000000000", "", nil)
	if h, err := c.hotAddProxy(ctx, func(string, ...any) {}); h != nil || err == nil || !strings.Contains(err.Error(), "not found as a VM") {
		t.Fatalf("not a VM of the host: %v %v", h, err)
	}
}

func TestSwapUUIDAndVMDir(t *testing.T) {
	if got := vmDir("[ds1] web/web_1.vmdk"); got != "[ds1] web" {
		t.Errorf("vmDir = %q", got)
	}
}
