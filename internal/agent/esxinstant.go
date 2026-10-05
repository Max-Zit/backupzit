package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/instantnfs"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmware"
)

// Instant recovery on VMware ESXi: the proxy agent (Linux) serves the VMs
// over NFS from esxiInstantDir; each running VM has a configuration file in
// vms/ and a directory in export/.

const (
	esxiInstantDir  = "/var/lib/backupzit/instant-esxi"
	esxiInstantUnit = "backupzit-esxi-nfs"
)

// esxiInstant describes a VM running from a backup on an ESXi host.
type esxiInstant struct {
	Dir       string               `json:"dir"`
	Name      string               `json:"name"`
	Snapshot  string               `json:"snapshot"`
	VMID      int                  `json:"vmid"`
	Repo      api.Repository       `json:"repo"`
	Disks     []vmware.InstantDisk `json:"disks"`
	Allow     []string             `json:"allow"` // ESXi host addresses
	Host      string               `json:"host"`
	Datastore string               `json:"datastore"`
	MoID      string               `json:"moid,omitempty"`
	Run       int64                `json:"run"`
}

func esxiPath(parts ...string) string {
	return filepath.Join(append([]string{esxiInstantDir}, parts...)...)
}

func loadESXiInstants() []esxiInstant {
	files, _ := filepath.Glob(esxiPath("vms", "*.json"))
	var out []esxiInstant
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var c esxiInstant
		if json.Unmarshal(b, &c) == nil {
			out = append(out, c)
		}
	}
	return out
}

func saveESXiInstant(c esxiInstant) error {
	if err := os.MkdirAll(esxiPath("vms"), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := esxiPath("vms", c.Dir+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, esxiPath("vms", c.Dir+".json"))
}

func removeESXiInstant(c esxiInstant) {
	os.Remove(esxiPath("vms", c.Dir+".json"))
	os.RemoveAll(esxiPath("vms", c.Dir))
	os.RemoveAll(esxiPath("export", c.Dir))
}

// ServeESXiInstant runs the NFS server of the instant VMs (the
// esxi-nfs-serve command, started as a systemd unit).
func ServeESXiInstant(ctx context.Context, logf func(string, ...any)) error {
	export := esxiPath("export")
	if err := os.MkdirAll(export, 0o700); err != nil {
		return err
	}
	type opened struct {
		r     *repo.Repository
		close func()
		sn    *repo.Snapshot
	}
	var mu sync.Mutex
	repos := map[string]*opened{}
	a := &Agent{}
	disk := func(name string) (*instantnfs.Overlay, error) {
		dir, flat, ok := strings.Cut(name, "/")
		if !ok || strings.Contains(flat, "/") {
			return nil, nil
		}
		var cfg *esxiInstant
		for _, c := range loadESXiInstants() {
			if c.Dir == dir {
				c := c
				cfg = &c
			}
		}
		if cfg == nil {
			return nil, nil
		}
		var d *vmware.InstantDisk
		for i := range cfg.Disks {
			if cfg.Disks[i].Flat == flat {
				d = &cfg.Disks[i]
			}
		}
		if d == nil {
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		op := repos[dir]
		if op == nil {
			r, closeRepo, err := a.openRepo(ctx, cfg.Repo, false)
			if err != nil {
				return nil, fmt.Errorf("open repository: %w", err)
			}
			sn, err := r.LoadSnapshot(ctx, cfg.Snapshot)
			if err != nil {
				closeRepo()
				return nil, err
			}
			op = &opened{r: r, close: closeRepo, sn: sn}
			repos[dir] = op
		}
		img := op.sn.Images[d.Image]
		if len(img.Partitions) != 1 {
			return nil, fmt.Errorf("disk %s has no image", d.Key)
		}
		pr, err := imaging.NewPartitionReader(ctx, op.r, &img.Partitions[0])
		if err != nil {
			return nil, err
		}
		if err := os.MkdirAll(esxiPath("vms", dir), 0o700); err != nil {
			return nil, err
		}
		logf("serving disk %s of %s", d.Key, cfg.Name)
		return instantnfs.OpenOverlay(pr, pr.Size(), filepath.Join(export, dir, flat), esxiPath("vms", dir, flat+".map"))
	}
	fs := instantnfs.NewFS(export, disk)
	h, err := instantnfs.NewHandler(fs, export, esxiPath("handles.key"), esxiPath("handles.journal"))
	if err != nil {
		return err
	}
	var allowMu sync.Mutex
	var allowIPs []net.IP
	var allowAt time.Time
	h.Allowed = func(ip net.IP) bool {
		allowMu.Lock()
		defer allowMu.Unlock()
		if time.Since(allowAt) > 30*time.Second {
			allowIPs, allowAt = nil, time.Now()
			for _, c := range loadESXiInstants() {
				for _, host := range c.Allow {
					if ips, err := net.LookupIP(host); err == nil {
						allowIPs = append(allowIPs, ips...)
					}
				}
			}
		}
		for _, a := range allowIPs {
			if a.Equal(ip) {
				return true
			}
		}
		return false
	}
	// Close the disks of VMs that were finished or discarded.
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			live := map[string]bool{}
			for _, c := range loadESXiInstants() {
				live[c.Dir] = true
			}
			mu.Lock()
			for dir, op := range repos {
				if !live[dir] {
					fs.Forget(dir)
					op.close()
					delete(repos, dir)
					logf("stopped serving %s", dir)
				}
			}
			mu.Unlock()
		}
	}()
	defer instantnfs.Unregister()
	logf("serving instant VMs from %s on port %d", export, instantnfs.Port)
	return instantnfs.Serve(ctx, h, logf)
}

// startESXiNFS starts the NFS server unit unless it runs, and waits until
// it answers.
func startESXiNFS(ctx context.Context) error {
	if exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", esxiInstantUnit).Run() != nil {
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		exec.CommandContext(ctx, "systemctl", "reset-failed", esxiInstantUnit).Run()
		out, err := exec.CommandContext(ctx, "systemd-run", "--unit", esxiInstantUnit, "--description", "BackupZit instant recovery NFS server for VMware ESXi",
			"--property=Restart=on-failure", exe, "esxi-nfs-serve").CombinedOutput()
		if err != nil {
			return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	for i := 0; i < 50; i++ {
		if c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", instantnfs.Port), time.Second); err == nil {
			c.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := exec.Command("journalctl", "-u", esxiInstantUnit, "-n", "5", "--no-pager").CombinedOutput()
	return fmt.Errorf("the NFS server did not start (port %d in use?): %s", instantnfs.Port, strings.TrimSpace(string(out)))
}

// localAddrTo is this machine's address on the way to host.
func localAddrTo(host string) (string, error) {
	c, err := net.DialTimeout("udp", net.JoinHostPort(strings.Split(host, ":")[0], "443"), 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP.String(), nil
}

// vmwareInstant starts, finishes or discards an instant VM on an ESXi host.
func (a *Agent) vmwareInstant(ctx context.Context, run api.Run) api.RunResult {
	if runtime.GOOS != "linux" {
		return failed(errors.New("instant recovery on VMware ESXi needs a Linux proxy agent (it serves the disks over NFS)"))
	}
	o := run.VMRestore
	if o == nil {
		return failed(errors.New("options missing"))
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	c, err := vmwareConnect(ctx, run.VMware)
	if err != nil {
		return failed(err)
	}
	defer c.Logout()
	if run.Kind != api.KindVMInstant {
		var cfg *esxiInstant
		for _, x := range loadESXiInstants() {
			if x.MoID == strconv.Itoa(o.InstantVMID) {
				x := x
				cfg = &x
			}
		}
		if cfg == nil {
			return failed(fmt.Errorf("instant VM %d is not served by this agent", o.InstantVMID))
		}
		var msg string
		if run.Kind == api.KindVMInstantFinish {
			id, err := c.InstantFinish(ctx, cfg.Datastore, cfg.Dir, cfg.MoID, o.Storage, logf)
			if err != nil {
				return failed(err)
			}
			msg = fmt.Sprintf("VM %q now runs from datastore %s (VM ID %s); the backup is no longer used", cfg.Name, o.Storage, id)
		} else {
			if err := c.InstantDiscard(ctx, cfg.MoID); err != nil {
				return failed(err)
			}
			msg = fmt.Sprintf("VM %q stopped and deleted", cfg.Name)
		}
		removeESXiInstant(*cfg)
		if len(loadESXiInstants()) == 0 {
			if err := c.InstantUnmount(ctx, cfg.Datastore); err != nil {
				msg += "; the datastore " + cfg.Datastore + " could not be removed: " + err.Error()
			}
			exec.CommandContext(ctx, "systemctl", "stop", esxiInstantUnit).Run()
		}
		return api.RunResult{Status: api.StatusSuccess, Message: msg}
	}

	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	closeRepo()
	if err != nil {
		return failed(err)
	}
	var g *repo.Guest
	for i := range sn.Guests {
		if sn.Guests[i].Platform == "vmware" && sn.Guests[i].VMID == o.VMID {
			g = &sn.Guests[i]
		}
	}
	if g == nil {
		return failed(fmt.Errorf("VM %d is not in the backup", o.VMID))
	}
	name := o.Name
	if name == "" {
		name = g.Name + "-instant"
	}
	asNew := c.VMExists(ctx, g.ID)
	dir := strings.Trim(badDirChars.ReplaceAllString(name, "_"), "._")
	if dir == "" {
		dir = "vm"
	}
	base := dir
	for i := 2; ; i++ {
		if _, err := os.Stat(esxiPath("export", dir)); os.IsNotExist(err) {
			break
		}
		dir = fmt.Sprintf("%s_%d", base, i)
	}
	plan, err := vmware.InstantPrepare(sn, o.VMID, esxiPath("export", dir), name, asNew)
	if err != nil {
		os.RemoveAll(esxiPath("export", dir))
		return failed(err)
	}
	cfg := esxiInstant{Dir: dir, Name: name, Snapshot: sn.ID.String(), VMID: o.VMID, Repo: run.Repository, Disks: plan.Disks,
		Allow: []string{strings.Split(run.VMware.Address, ":")[0]}, Host: run.VMware.Name,
		Datastore: vmware.InstantDatastore(hostnameShort()), Run: run.ID}
	if err := saveESXiInstant(cfg); err != nil {
		return failed(err)
	}
	fail := func(err error) api.RunResult {
		removeESXiInstant(cfg)
		return failed(err)
	}
	if err := startESXiNFS(ctx); err != nil {
		return fail(err)
	}
	ip, err := localAddrTo(run.VMware.Address)
	if err != nil {
		return fail(fmt.Errorf("no route to %s: %w", run.VMware.Address, err))
	}
	logf("starting instant VM on ESXi", "name", name, "datastore", cfg.Datastore, "nfs", ip)
	moid, notes, err := c.InstantStart(ctx, cfg.Datastore, ip, dir, name, true)
	if err != nil {
		return fail(err)
	}
	cfg.MoID = moid
	if err := saveESXiInstant(cfg); err != nil {
		return failed(err)
	}
	notes = append(plan.Notes, notes...)
	msg := fmt.Sprintf("VM %q runs from the backup on %s (datastore %s, served by this agent)", name, run.VMware.Name, cfg.Datastore)
	if len(notes) > 0 {
		msg += ". " + strings.Join(notes, "; ")
	}
	var disks []string
	for _, d := range plan.Disks {
		disks = append(disks, "["+cfg.Datastore+"] "+dir+"/"+strings.TrimSuffix(d.Flat, "-flat.vmdk")+".vmdk")
	}
	id, _ := strconv.Atoi(moid)
	details, _ := json.Marshal(map[string]any{"instant_vmid": id, "node": run.VMware.Name, "name": name, "state": "running", "disks": disks, "platform": "vmware"})
	return api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details}
}

var badDirChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func hostnameShort() string {
	h, _ := os.Hostname()
	return h
}
