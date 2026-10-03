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
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/vmfs"
)

// cachedVMDisk keeps the volumes and opened file systems of a guest disk.
type cachedVMDisk struct {
	vols  []vmfs.Volume
	fss   map[string]vmfs.FS
	disk  string
	disks []string
	used  time.Time
}

// vmDisk opens a guest disk of a VM backup run (cached for browsing).
func (s *Server) vmDisk(ctx context.Context, run Run, vmid int, disk string) (*cachedVMDisk, error) {
	if run.Kind != api.KindVMBackup || run.SnapshotID == "" || run.Expired {
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
	if t.SFTPKey != "" {
		return nil, errors.New("browsing storage that uses SSH key authentication is not supported yet")
	}
	r, err := s.cache.repo(ctx, t, run.RepoURL)
	if err != nil {
		return nil, err
	}
	bg := context.WithoutCancel(ctx)
	sn, err := r.LoadSnapshot(bg, run.SnapshotID)
	if err != nil {
		return nil, err
	}
	rd, size, d, err := vmfs.GuestDisk(bg, r, sn, vmid, disk)
	if err != nil {
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
	r.ParseForm()
	p := vmBrowseParams{run: run, disk: r.FormValue("disk"), volume: r.FormValue("vol"), dir: vmfs.Clean(r.FormValue("path"))}
	p.vmid, err = strconv.Atoi(r.FormValue("vmid"))
	if err != nil {
		return p, errors.New("choose a VM or container")
	}
	return p, nil
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
	s.audit(r, "restore.download", "%d paths from guest %d (%s) of backup #%d", len(sel), p.vmid, v.ID, p.run.ID)
	if len(sel) == 1 {
		if e, err := f.Stat(sel[0]); err == nil && !e.IsDir {
			rd, size, err := f.ReadFile(vmfs.Clean(sel[0]))
			if err != nil {
				redirectErr(w, r, vmBack(p), err)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(vmfs.Base(sel[0])))
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			io.Copy(w, rd)
			return
		}
	}
	name := fmt.Sprintf("vm%d-%s.zip", p.vmid, time.Now().Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	zw := zip.NewWriter(w)
	defer zw.Close()
	base := p.dir
	var add func(e vmfs.Entry) error
	add = func(e vmfs.Entry) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(e.Path, base), "/")
		if rel == "" {
			rel = vmfs.Base(e.Path)
		}
		if e.IsDir {
			children, err := f.List(e.Path)
			if err != nil {
				return nil
			}
			if len(children) == 0 {
				zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: e.ModTime})
			}
			for _, ch := range children {
				if err := add(ch); err != nil {
					return err
				}
			}
			return nil
		}
		rd, _, err := f.ReadFile(e.Path)
		if err != nil {
			return nil
		}
		zf, err := zw.CreateHeader(&zip.FileHeader{Name: path.Clean(rel), Method: zip.Deflate, Modified: e.ModTime})
		if err != nil {
			return err
		}
		_, err = io.Copy(zf, rd)
		return err
	}
	for _, sp := range sel {
		e, err := f.Stat(sp)
		if err != nil {
			continue
		}
		e.Path = vmfs.Clean(sp)
		if add(e) != nil {
			return
		}
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
	if b.Kind != api.KindVMBackup || b.SnapshotID == "" || b.Expired {
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
