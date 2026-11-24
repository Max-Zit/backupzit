package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
	"github.com/max-zit/backupzit/internal/vmfs"
)

// cachedVMDisk keeps the volumes and opened file systems of a guest disk.
type cachedVMDisk struct {
	vols []vmfs.Volume
	// mu guards fss: two browse or download requests of the same disk at
	// once used to write the map together ("concurrent map writes" ends
	// the whole console).
	mu    sync.Mutex
	fss   map[string]vmfs.FS
	disk  string
	disks []string
	used  time.Time
}

// vmDisk opens a guest disk of a VM backup run (cached for browsing).
func (s *Server) vmDisk(ctx context.Context, run Run, vmid int, disk string) (*cachedVMDisk, error) {
	if vmDetails(run) == nil || run.SnapshotID == "" || run.Expired {
		return nil, errors.New("run has no VM backup to browse")
	}
	if run.TargetID == nil {
		return nil, errors.New("storage target was deleted")
	}
	key := fmt.Sprintf("%s/%d/%s", run.SnapshotID, vmid, disk)
	s.cache.mu.Lock()
	if s.cache.vm == nil {
		s.cache.vm = map[string]*cachedVMDisk{}
	}
	for k, c := range s.cache.vm {
		if time.Since(c.used) > cacheTTL {
			delete(s.cache.vm, k)
		}
	}
	if c, ok := s.cache.vm[key]; ok {
		c.used = time.Now()
		s.cache.mu.Unlock()
		return c, nil
	}
	s.cache.mu.Unlock()
	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return nil, errors.New("storage target was deleted")
	}
	if err := browseRefused(t); err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	var sn *repo.Snapshot
	var rd io.ReaderAt
	var size int64
	var d *repo.GuestDisk
	if err := s.openRunSnapshot(ctx, t, run, func(r *repo.Repository, s2 *repo.Snapshot) (err error) {
		sn = s2
		rd, size, d, err = vmfs.GuestDisk(bg, r, s2, vmid, disk)
		return err
	}); err != nil {
		return nil, err
	}
	vols, err := vmfs.Volumes(rd, size)
	if err != nil {
		return nil, err
	}
	c := &cachedVMDisk{vols: vols, fss: map[string]vmfs.FS{}, disk: d.Key, disks: vmfs.GuestDisks(sn, vmid), used: time.Now()}
	s.cache.mu.Lock()
	s.cache.vm[key] = c
	s.cache.mu.Unlock()
	return c, nil
}

func (c *cachedVMDisk) fs(volID string) (vmfs.Volume, vmfs.FS, error) {
	v, err := vmfs.FindVolume(c.vols, volID)
	if err != nil {
		return v, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if f, ok := c.fss[v.ID]; ok {
		return v, f, nil
	}
	f, err := vmfs.Open(v)
	if err != nil {
		return v, nil, err
	}
	c.fss[v.ID] = f
	return v, f, nil
}

type vmBrowseParams struct {
	run    Run
	vmid   int
	disk   string
	volume string
	dir    string
}

func (s *Server) vmBrowseParams(r *http.Request) (vmBrowseParams, error) {
	id, err := pathID(r)
	if err != nil {
		return vmBrowseParams{}, ErrNotFound
	}
	run, err := s.store.GetRun(r.Context(), id)
	if err != nil {
		return vmBrowseParams{}, err
	}
	_ = r.ParseForm()
	p := vmBrowseParams{run: run, disk: r.FormValue("disk"), volume: r.FormValue("vol"), dir: vmfs.Clean(r.FormValue("path"))}
	p.vmid, err = strconv.Atoi(r.FormValue("vmid"))
	if err != nil {
		return p, errors.New("choose a VM or container")
	}
	return p, nil
}

// hasVolume tells whether id names one of vols.
func hasVolume(vols []vmfs.Volume, id string) bool {
	for _, v := range vols {
		if v.ID == id {
			return true
		}
	}
	return false
}

func vmCrumbs(dir string) []crumb {
	out := []crumb{{Name: "/", Path: "/"}}
	cur := ""
	for _, s := range strings.Split(strings.Trim(dir, "/"), "/") {
		if s == "" {
			continue
		}
		cur += "/" + s
		out = append(out, crumb{Name: s, Path: cur})
	}
	return out
}

func (s *Server) handleVMBrowse(w http.ResponseWriter, r *http.Request, user string) {
	p, err := s.vmBrowseParams(r)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if d := vmDetails(p.run); d != nil {
		for _, g := range d.Guests {
			// LXC containers on ZFS are files in the snapshot's tree, not disks.
			if g.VMID == p.vmid && g.Files > 0 && g.Path != "" {
				http.Redirect(w, r, fileBack(p.run, g.Path), http.StatusSeeOther)
				return
			}
		}
	}
	data := map[string]any{"Run": p.run, "VMID": p.vmid, "Path": p.dir, "Crumbs": vmCrumbs(p.dir)}
	// Proxmox guests are known by their ID, Hyper-V and VMware VMs by name only.
	label := fmt.Sprintf("guest %d", p.vmid)
	if d := vmDetails(p.run); d != nil {
		for _, g := range d.Guests {
			if g.VMID == p.vmid {
				data["GuestName"] = g.Name
				if g.Platform == "hyperv" || g.Platform == "vmware" {
					label = "VM " + g.Name
				} else {
					label = fmt.Sprintf("guest %d — %s", p.vmid, g.Name)
				}
			}
		}
	}
	data["Label"] = label
	pd := pageData{Title: "Files of " + label, Nav: "runs", User: user, Data: data}
	if err == nil {
		var c *cachedVMDisk
		c, err = s.vmDisk(r.Context(), p.run, p.vmid, p.disk)
		if err == nil {
			data["Disk"], data["Disks"], data["Volumes"] = c.disk, c.disks, c.vols
			var v vmfs.Volume
			var f vmfs.FS
			v, f, err = c.fs(p.volume)
			if err != nil && p.volume != "" && !hasVolume(c.vols, p.volume) {
				// The form sends the partition of the disk shown before
				// when only the disk is changed ("no volume p3", B10):
				// start at the first browsable partition of this disk.
				p.volume, p.dir = "", "/"
				data["Path"], data["Crumbs"] = p.dir, vmCrumbs(p.dir)
				v, f, err = c.fs("")
			}
			data["Volume"] = v
			if err == nil {
				var entries []vmfs.Entry
				entries, err = f.List(p.dir)
				data["Entries"] = entries
			}
		}
	}
	if err != nil {
		pd.Error = err.Error()
	}
	agents, aerr := s.store.ListAgents(r.Context())
	if aerr != nil {
		s.serverError(w, aerr)
		return
	}
	data["Agents"] = agents
	s.render(w, r, "vmbrowse", pd)
}

func vmBack(p vmBrowseParams) string {
	return fmt.Sprintf("/runs/%d/vmbrowse?", p.run.ID) + url.Values{"vmid": {strconv.Itoa(p.vmid)}, "disk": {p.disk}, "vol": {p.volume}, "path": {p.dir}}.Encode()
}

// handleVMDownload streams the ticked files: one file as is, several files
// or folders as a ZIP archive.
func (s *Server) handleVMDownload(w http.ResponseWriter, r *http.Request, user string) {
	p, err := s.vmBrowseParams(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sel := r.Form["sel"]
	if len(sel) == 0 {
		redirectErr(w, r, vmBack(p), errors.New("tick the files and folders to download"))
		return
	}
	c, err := s.vmDisk(r.Context(), p.run, p.vmid, p.disk)
	if err != nil {
		redirectErr(w, r, vmBack(p), err)
		return
	}
	v, f, err := c.fs(p.volume)
	if err != nil {
		redirectErr(w, r, vmBack(p), err)
		return
	}
	// Find every ticked entry before anything is sent: a read error must
	// show as a message, not as a broken download.
	for _, sp := range sel {
		if _, err := f.Stat(sp); err != nil {
			redirectErr(w, r, vmBack(p), err)
			return
		}
	}
	s.audit(r, "restore.download", "%d paths from guest %d (%s) of backup #%d: %s", len(sel), p.vmid, v.ID, p.run.ID, auditPaths(sel))
	if len(sel) == 1 {
		if e, err := f.Stat(sel[0]); err == nil && !e.IsDir {
			rd, size, err := f.ReadFile(vmfs.Clean(sel[0]))
			if err != nil {
				redirectErr(w, r, vmBack(p), err)
				return
			}
			setAttachment(w, vmfs.Base(sel[0]), "application/octet-stream", size)
			if _, err := io.Copy(w, rd); err != nil {
				s.abortDownload("guest file", err)
			}
			return
		}
	}
	name := fmt.Sprintf("vm%d-%s.zip", p.vmid, time.Now().Format("20060102-1504"))
	setAttachment(w, name, "application/zip", -1)
	zw := zip.NewWriter(w)
	base := p.dir
	// A folder already in the archive (a loop in a damaged or crafted file
	// system) is not walked again (P6).
	seen := vmfs.Visited{}
	var add func(e vmfs.Entry, depth int) error
	add = func(e vmfs.Entry, depth int) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if depth > vmfs.MaxDepth {
			return fmt.Errorf("%s: more than %d folders deep (damaged file system?)", e.Path, vmfs.MaxDepth)
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, base), "/")
		if rel == "" {
			rel = vmfs.Base(e.Path)
		}
		rel = zipEntryName(rel)
		if e.IsDir {
			if !seen.Enter(e.ID) {
				return nil
			}
			children, err := f.List(e.Path)
			if err != nil {
				return err
			}
			if len(children) == 0 {
				_, _ = zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: e.ModTime})
			}
			for _, ch := range children {
				if err := add(ch, depth+1); err != nil {
					return err
				}
			}
			return nil
		}
		rd, _, err := f.ReadFile(e.Path)
		if err != nil {
			return err
		}
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: e.ModTime})
		if err != nil {
			return err
		}
		_, err = io.Copy(zf, rd)
		return err
	}
	for _, sp := range sel {
		e, err := f.Stat(sp)
		if err == nil {
			e.Path = vmfs.Clean(sp)
			seen = vmfs.Visited{}
			err = add(e, 0)
		}
		if err != nil {
			s.abortDownload("guest archive", err)
		}
	}
	if err := zw.Close(); err != nil {
		s.abortDownload("guest archive", err)
	}
}

func (s *Server) handleVMFilesRestore(w http.ResponseWriter, r *http.Request, _ string) {
	p, err := s.vmBrowseParams(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	back := vmBack(p)
	sel := r.Form["sel"]
	target := strings.TrimSpace(r.FormValue("target"))
	if len(sel) == 0 || target == "" {
		redirectErr(w, r, back, errors.New("tick files or folders and enter the folder to restore into"))
		return
	}
	c, err := s.vmDisk(r.Context(), p.run, p.vmid, p.disk)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	v, _, err := c.fs(p.volume)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	for i := range sel {
		sel[i] = vmfs.Clean(sel[i])
	}
	rid, err := s.store.QueueVMFileRestore(r.Context(), p.run.ID, formID(r, "agent_id"),
		api.VMFileRestore{VMID: p.vmid, Disk: c.disk, Volume: v.ID, Paths: sel, Target: target})
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.vmfiles", "run #%d: %d paths from guest %d of backup #%d to %q", rid, len(sel), p.vmid, p.run.ID, target)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "File restore queued.")
}

// QueueVMFileRestore creates a run that copies files out of a guest disk.
func (s *Store) QueueVMFileRestore(ctx context.Context, backupRunID, agentID int64, o api.VMFileRestore) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if vmDetails(b) == nil || b.SnapshotID == "" || b.Expired {
		return 0, errors.New("run has no VM backup")
	}
	if _, err := s.GetAgent(ctx, agentID); err != nil {
		return 0, errors.New("unknown agent")
	}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, paths, restore_target, vm_restore)
		VALUES($1,$2,'vm-file-restore','manual',$3,$4,$5,$6,$7,$8) RETURNING id`,
		agentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, o.Paths, o.Target, opts).Scan(&id)
	return id, err
}
