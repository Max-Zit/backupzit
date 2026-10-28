package pve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/nbd"
)

// fakeMonitor is a VM's QEMU monitor: disks with their bitmaps.
type fakeMonitor struct {
	devs    []blockDevice
	bitmaps map[string][]string // bitmap node → names
	log     []string
	nbdOn   bool
	exports map[string][]string // export → bitmaps
	// graphChange moves scsi0 to a new node (a snapshot as volume chain).
	graphChange bool
}

func (m *fakeMonitor) rootOf(node string) int {
	for i, d := range m.devs {
		if d.Device == node || d.Inserted != nil && d.Inserted.NodeName == node {
			return i
		}
	}
	return -1
}

func (m *fakeMonitor) run(c qmpCmd) (any, error) {
	a := c.Arguments
	m.log = append(m.log, c.Execute)
	switch c.Execute {
	case "query-block":
		out := make([]map[string]any, len(m.devs))
		for i, d := range m.devs {
			if d.Inserted == nil {
				out[i] = map[string]any{"device": d.Device, "qdev": d.QDev}
				continue
			}
			key := d.Device
			if key == "" {
				key = d.Inserted.NodeName
			}
			var bs []map[string]string
			for _, n := range m.bitmaps[key] {
				bs = append(bs, map[string]string{"name": n})
			}
			out[i] = map[string]any{"device": d.Device, "qdev": d.QDev, "inserted": map[string]any{"node-name": d.Inserted.NodeName, "dirty-bitmaps": bs}}
		}
		return out, nil
	case "transaction":
		for _, x := range a["actions"].([]any) {
			act := x.(map[string]any)
			data := act["data"].(map[string]any)
			node := data["node"].(string)
			if act["type"] != "block-dirty-bitmap-add" || m.rootOf(node) < 0 || data["persistent"] != false {
				return nil, errors.New("bad transaction")
			}
			m.bitmaps[node] = append(m.bitmaps[node], data["name"].(string))
		}
		if m.graphChange {
			m.devs[0].Device, m.devs[0].Inserted.NodeName = "", "#block99"
		}
		return map[string]any{}, nil
	case "block-dirty-bitmap-remove":
		node, name := a["node"].(string), a["name"].(string)
		i := slices.Index(m.bitmaps[node], name)
		if i < 0 {
			return nil, fmt.Errorf("Dirty bitmap '%s' not found", name)
		}
		m.bitmaps[node] = slices.Delete(m.bitmaps[node], i, i+1)
		m.log[len(m.log)-1] += " " + name
		return map[string]any{}, nil
	case "nbd-server-start":
		if m.nbdOn {
			return nil, errors.New("NBD server already running")
		}
		m.nbdOn = true
		return map[string]any{}, nil
	case "nbd-server-stop":
		m.nbdOn = false
		return map[string]any{}, nil
	case "block-export-add":
		if !m.nbdOn || a["writable"] != false {
			return nil, errors.New("bad export")
		}
		var bs []string
		for _, b := range a["bitmaps"].([]any) {
			bs = append(bs, b.(string))
		}
		m.exports[a["name"].(string)] = bs
		return map[string]any{}, nil
	case "block-export-del":
		delete(m.exports, a["id"].(string))
		return map[string]any{}, nil
	}
	return nil, fmt.Errorf("unknown command %s", c.Execute)
}

func newFakeMonitor(t *testing.T) *fakeMonitor {
	inserted := func(node string) *struct {
		NodeName string `json:"node-name"`
		Bitmaps  []struct {
			Name string `json:"name"`
		} `json:"dirty-bitmaps"`
	} {
		x := &struct {
			NodeName string `json:"node-name"`
			Bitmaps  []struct {
				Name string `json:"name"`
			} `json:"dirty-bitmaps"`
		}{NodeName: node}
		return x
	}
	m := &fakeMonitor{bitmaps: map[string][]string{}, exports: map[string][]string{},
		devs: []blockDevice{
			{Device: "drive-scsi0", QDev: "scsi0", Inserted: inserted("#block123")},                               // Proxmox VE 8 (-drive)
			{Device: "", QDev: "/machine/peripheral/virtio1/virtio-backend", Inserted: inserted("drive-virtio1")}, // Proxmox VE 9 (-blockdev)
			{Device: "", QDev: "/machine/peripheral/sata2/x", Inserted: inserted("#block7")},                      // generated name: not tracked
			{Device: "drive-ide2", QDev: "ide2", Inserted: nil},                                                   // empty CD drive
		}}
	old := qmp
	qmp = func(_ context.Context, _ int, cmds ...qmpCmd) ([]qmpResult, error) {
		// Through JSON, as the real monitor client gets them.
		b, _ := json.Marshal(cmds)
		var in []qmpCmd
		json.Unmarshal(b, &in)
		var out []qmpResult
		for _, c := range in {
			v, err := m.run(c)
			if err != nil {
				out = append(out, qmpResult{Error: err.Error() + " at /usr/share/perl5/PVE/QMPClient.pm line 1.\n"})
				continue
			}
			raw, _ := json.Marshal(v)
			out = append(out, qmpResult{Return: raw})
		}
		return out, nil
	}
	oldDir, oldAreas := nbdSocketDir, dirtyAreas
	nbdSocketDir = t.TempDir()
	dirtyAreas = func(_ context.Context, addr, export, bitmap string) ([]nbd.Area, uint64, error) {
		if !strings.HasPrefix(addr, "unix:"+nbdSocketDir) || !slices.Contains(m.exports[export], bitmap) {
			return nil, 0, errors.New("the server has no qemu:dirty-bitmap:" + bitmap)
		}
		return []nbd.Area{{Offset: 1 << 20, Length: 65536}}, 8 << 30, nil
	}
	t.Cleanup(func() { qmp, nbdSocketDir, dirtyAreas = old, oldDir, oldAreas })
	return m
}

func TestChangedBlockTracking(t *testing.T) {
	m := newFakeMonitor(t)
	ctx := context.Background()
	logf := func(string, ...any) {}
	keys := []string{"scsi0", "virtio1", "sata2"}

	// Earlier bitmaps: one of the previous backup, one left 70 days ago.
	prev := fmt.Sprintf("backupzit-%d-aaaa", time.Now().Add(-24*time.Hour).Unix())
	stale := fmt.Sprintf("backupzit-%d-bbbb", time.Now().Add(-70*24*time.Hour).Unix())
	m.bitmaps["drive-scsi0"] = []string{prev, stale, "pbs-incremental-dirty-bitmap"}

	tr, err := startTracking(ctx, 100, keys, logf)
	if err != nil {
		t.Fatal(err)
	}
	if !tr.tracking("scsi0") || !tr.tracking("virtio1") || tr.tracking("sata2") {
		t.Fatalf("tracked disks: %v", tr.disks)
	}
	if !slices.Equal(m.bitmaps["drive-scsi0"], []string{prev, "pbs-incremental-dirty-bitmap", tr.name}) {
		t.Fatalf("scsi0 bitmaps: %v (stale one not removed, other software's kept)", m.bitmaps["drive-scsi0"])
	}
	if !slices.Equal(m.bitmaps["drive-virtio1"], []string{tr.name}) {
		t.Fatalf("virtio1 bitmaps: %v", m.bitmaps["drive-virtio1"])
	}
	tr.verify(ctx)
	if !tr.tracking("scsi0") || !tr.tracking("virtio1") {
		t.Fatal("verify dropped a disk")
	}

	// scsi0: only the changed area; virtio1 has no previous bitmap.
	areas, size, err := tr.changes(ctx, "scsi0", prev)
	if err != nil || size != 8<<30 || !reflect.DeepEqual(areas, []diskimg.Area{{Offset: 1 << 20, Length: 65536}}) {
		t.Fatalf("changes: %v %d %v", areas, size, err)
	}
	if _, _, err := tr.changes(ctx, "virtio1", prev); err == nil {
		t.Fatal("changes without the previous bitmap")
	}
	if len(m.exports) != 0 {
		t.Fatalf("exports left: %v", m.exports)
	}
	tr.stopServer()
	if m.nbdOn {
		t.Fatal("NBD server left running")
	}
	// Saved: the previous bitmap goes, this backup's stays for the next.
	tr.finish(true)
	if !slices.Equal(m.bitmaps["drive-scsi0"], []string{"pbs-incremental-dirty-bitmap", tr.name}) || !slices.Equal(m.bitmaps["drive-virtio1"], []string{tr.name}) {
		t.Fatalf("after a saved backup: %v", m.bitmaps)
	}

	// A failed backup removes its own bitmap and keeps the previous one.
	tr2, err := startTracking(ctx, 100, keys, logf)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := tr2.changes(ctx, "scsi0", tr.name); err != nil {
		t.Fatal(err)
	}
	tr2.finish(false)
	if !slices.Equal(m.bitmaps["drive-scsi0"], []string{"pbs-incremental-dirty-bitmap", tr.name}) {
		t.Fatalf("after a failed backup: %v", m.bitmaps)
	}

	// A snapshot that moves the disk to a new node: tracking stops for it.
	m.graphChange = true
	tr3, err := startTracking(ctx, 100, keys, logf)
	if err != nil {
		t.Fatal(err)
	}
	tr3.verify(ctx)
	if tr3.tracking("scsi0") || !tr3.tracking("virtio1") {
		t.Fatalf("after a block graph change: %v", tr3.disks)
	}
}

func TestPreviousDisksKey(t *testing.T) {
	for _, c := range []struct {
		d    blockDevice
		want string
	}{
		{blockDevice{Device: "drive-scsi0"}, "scsi0"},
		{blockDevice{QDev: "/machine/peripheral/virtio12/virtio-backend"}, "virtio12"},
		{blockDevice{Device: "drive-efidisk0"}, ""},
		{blockDevice{Device: "drive-sata1", QDev: "sata1"}, "sata1"},
	} {
		if got := diskKeyOf(c.d); got != c.want {
			t.Errorf("%+v: %q", c.d, got)
		}
	}
	if !bitmapTime("backupzit-1790000000-ab12").Equal(time.Unix(1790000000, 0)) || !bitmapTime("other").IsZero() {
		t.Error("bitmap time")
	}
}
