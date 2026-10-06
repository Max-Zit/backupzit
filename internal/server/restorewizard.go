package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

// The restore wizard guides through: what to restore → which machine, job
// or VM → which restore point → restore options. Every step is a plain GET
// page, so steps can be bookmarked and the browser's back button works; the
// options step submits the same forms as the run page.

// restoreType is one choice of the wizard's first step.
type restoreType struct {
	Key, Title, Desc, Icon string
	// Count is the number of restore points available for it.
	Count int
}

var restoreTypes = []restoreType{
	{Key: "vm", Title: "Whole virtual machine", Desc: "Proxmox VE, Hyper-V and VMware ESXi VMs (and Proxmox containers), as a new VM or in place of the original.", Icon: "vm"},
	{Key: "vmfiles", Title: "Files from a virtual machine", Desc: "Browse the disks of a VM backup (NTFS, ext4, XFS, LVM), download files or restore them to an agent.", Icon: "folder"},
	{Key: "files", Title: "Files and folders", Desc: "From file backups and Linux system backups, into a folder or to their original location.", Icon: "folder"},
	{Key: "image", Title: "Disk image / bare metal", Desc: "A Windows disk or its partitions onto a disk, also on different hardware or as a VM, from the recovery ISO.", Icon: "disk"},
	{Key: "imagefiles", Title: "Files from a disk image", Desc: "Browse the NTFS partitions of a disk image and restore single files.", Icon: "folder"},
	{Key: "sql", Title: "Database", Desc: "A SQL Server, PostgreSQL or MySQL/MariaDB database (or a whole PostgreSQL cluster), under a new name or in place, to any point in time covered by log backups.", Icon: "storage"},
	{Key: "system", Title: "Whole Linux system", Desc: "Onto an empty disk or bare metal (Linux recovery ISO), or as a new Proxmox VM (P2V).", Icon: "agents"},
}

// restoreSource is one choice of the second step: a job, or a single VM
// across the VM backup jobs.
type restoreSource struct {
	Key      string
	Title    string
	Detail   string
	Platform string
	Latest   time.Time
	Count    int
}

// restorePoint is one backup the user can restore from.
type restorePoint struct {
	Run    Run
	Size   uint64
	Target string
	// Copy marks backups in a backup copy job (secondary storage).
	Copy bool
	// Guest is the VM within a VM backup.
	Guest string
}

func restoreTypeByKey(k string) (restoreType, bool) {
	for _, t := range restoreTypes {
		if t.Key == k {
			return t, true
		}
	}
	return restoreType{}, false
}

// ListRestorePoints returns the successful, not expired backups, newest first.
func (s *Store) ListRestorePoints(ctx context.Context) ([]Run, error) {
	rows, err := s.db.Query(ctx, `SELECT `+runCols+runFrom+` WHERE r.kind = ANY($1) AND COALESCE(r.snapshot_id,'') <> ''
		AND NOT r.expired AND r.status IN ('success','warning') ORDER BY r.queued_at DESC LIMIT 5000`,
		[]string{api.KindBackup, api.KindImageBackup, api.KindVMBackup, api.KindSystemBackup, api.KindCopy, api.KindSQLBackup})
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Run, error) { return scanRun(r) })
}

// wizardEntry is a restore point as one source of one restore type.
type wizardEntry struct {
	src   restoreSource
	point restorePoint
}

// wizardEntries expands the restore points into (type, source, point).
func (s *Server) wizardEntries(ctx context.Context) (map[string][]wizardEntry, error) {
	runs, err := s.store.ListRestorePoints(ctx)
	if err != nil {
		return nil, err
	}
	jobs, err := s.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	targets, err := s.store.ListTargets(ctx)
	if err != nil {
		return nil, err
	}
	jobByID := map[int64]Job{}
	for _, j := range jobs {
		jobByID[j.ID] = j
	}
	targetName := map[int64]string{}
	for _, t := range targets {
		targetName[t.ID] = t.Name
	}
	out := map[string][]wizardEntry{}
	for _, run := range runs {
		p := restorePoint{Run: run, Target: run.RepoURL}
		if run.TargetID != nil && targetName[*run.TargetID] != "" {
			p.Target = targetName[*run.TargetID]
		}
		var st repo.SnapshotStats
		if json.Unmarshal(run.Stats, &st) == nil {
			p.Size = st.Bytes
			if run.Kind != api.KindBackup && run.Kind != api.KindSystemBackup {
				p.Size = st.BytesRead
			}
		}
		// A job's backups are one source; runs of deleted jobs are grouped
		// by machine.
		src := restoreSource{Key: "agent:" + strconv.FormatInt(run.AgentID, 10), Title: run.Hostname, Detail: "backups of a deleted job"}
		if run.JobID != nil {
			j := jobByID[*run.JobID]
			src = restoreSource{Key: "job:" + strconv.FormatInt(*run.JobID, 10), Title: deref(run.JobName), Detail: run.Hostname}
			if run.Kind == api.KindCopy {
				// Copies of file and VM backups are restore points of the source job.
				srcJob, ok := jobByID[derefID(j.SourceJobID)]
				if !ok || (srcJob.Kind != JobFiles && srcJob.Kind != JobNAS && srcJob.Kind != JobVM) {
					continue
				}
				if srcJob.Kind == JobVM && vmDetails(run) == nil {
					continue // copied before copies listed their guests
				}
				p.Copy = true
				src = restoreSource{Key: "job:" + strconv.FormatInt(srcJob.ID, 10), Title: srcJob.Name, Detail: srcJob.Hostname}
			}
		} else if run.Kind == api.KindCopy {
			continue
		}
		add := func(typ string, src restoreSource, p restorePoint) {
			out[typ] = append(out[typ], wizardEntry{src: src, point: p})
		}
		kind := run.Kind
		if kind == api.KindCopy {
			// A copy restores like what it copied.
			kind = api.KindBackup
			if vmDetails(run) != nil {
				kind = api.KindVMBackup
			}
		}
		switch kind {
		case api.KindBackup:
			add("files", src, p)
		case api.KindSQLBackup:
			d := sqlDetails(run)
			if d == nil {
				continue
			}
			server := "SQL Server " + d.Instance
			switch {
			case d.Engine == "postgres":
				server = "PostgreSQL"
			case d.Engine == "mysql":
				server = "MySQL/MariaDB"
			case d.Instance == "":
				server = "SQL Server default instance"
			}
			for _, db := range d.Databases {
				title := db.Name
				switch db.Name {
				case "(globals)":
					continue
				case "(cluster)":
					title = "Whole PostgreSQL cluster"
				}
				gp := p
				gp.Guest, gp.Size = title, db.Size
				add("sql", restoreSource{Key: "sql:" + run.Hostname + ":" + d.Instance + ":" + db.Name, Title: title,
					Detail: server + " on " + run.Hostname}, gp)
			}
		case api.KindSystemBackup:
			add("files", src, p)
			add("system", src, p)
		case api.KindImageBackup:
			add("image", src, p)
		browse:
			for _, img := range imageDetails(run) {
				for _, part := range img.Partitions {
					if part.Included && part.FileSystem == "NTFS" {
						add("imagefiles", src, p)
						break browse
					}
				}
			}
		case api.KindVMBackup:
			d := vmDetails(run)
			if d == nil {
				continue
			}
			for _, g := range d.Guests {
				platform := g.Platform
				if platform == "" {
					platform = "proxmox"
				}
				title := g.Name
				if platform == "proxmox" {
					title = fmt.Sprintf("%d — %s", g.VMID, g.Name)
				}
				kind := "VM"
				if g.Type == "lxc" {
					kind = "container"
				}
				vs := restoreSource{Key: fmt.Sprintf("vm:%s:%d", platform, g.VMID), Title: title,
					Detail: fmt.Sprintf("%s on %s", kind, run.Hostname), Platform: platformLabel(platform)}
				gp := p
				gp.Guest = g.Name
				gp.Size = g.Size
				add("vm", vs, gp)
				add("vmfiles", vs, gp)
			}
		}
	}
	return out, nil
}

func derefID(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func (s *Server) handleRestoreWizard(w http.ResponseWriter, r *http.Request, user string) {
	ctx := r.Context()
	q := r.URL.Query()
	entries, err := s.wizardEntries(ctx)
	if err != nil {
		s.serverError(w, err)
		return
	}
	data := map[string]any{"Step": 1}
	page := pageData{Title: "Restore", Nav: "restore", User: user, Data: data}

	// Step 1: what to restore.
	types := make([]restoreType, len(restoreTypes))
	copy(types, restoreTypes)
	for i := range types {
		seen := map[int64]bool{}
		for _, e := range entries[types[i].Key] {
			if !seen[e.point.Run.ID] {
				seen[e.point.Run.ID] = true
				types[i].Count++
			}
		}
	}
	data["Types"] = types
	typ, ok := restoreTypeByKey(q.Get("type"))
	if !ok {
		s.render(w, r, "restore", page)
		return
	}
	data["Type"], data["Step"] = typ, 2

	// Step 2: which job, machine or VM.
	bySrc := map[string]*restoreSource{}
	var sources []*restoreSource
	for _, e := range entries[typ.Key] {
		src, ok := bySrc[e.src.Key]
		if !ok {
			c := e.src
			src = &c
			bySrc[c.Key] = src
			sources = append(sources, src)
		}
		src.Count++
		if t := e.point.Run.QueuedAt; t.After(src.Latest) {
			src.Latest = t
		}
	}
	sort.SliceStable(sources, func(i, j int) bool { return strings.ToLower(sources[i].Title) < strings.ToLower(sources[j].Title) })
	data["Sources"] = sources
	srcKey := q.Get("src")
	src, ok := bySrc[srcKey]
	if !ok {
		s.render(w, r, "restore", page)
		return
	}
	data["Source"], data["Step"] = src, 3

	// Step 3: which restore point.
	var points []restorePoint
	for _, e := range entries[typ.Key] {
		if e.src.Key == srcKey {
			points = append(points, e.point)
		}
	}
	data["Points"] = points
	var vmid int
	if strings.HasPrefix(srcKey, "vm:") {
		vmid, _ = strconv.Atoi(srcKey[strings.LastIndex(srcKey, ":")+1:])
	}
	data["VMID"] = vmid
	runID, _ := strconv.ParseInt(q.Get("run"), 10, 64)
	var point *restorePoint
	for i := range points {
		if points[i].Run.ID == runID {
			point = &points[i]
		}
	}
	if point == nil {
		s.render(w, r, "restore", page)
		return
	}

	// Step 4: options, with the forms of the run page.
	form, err := s.restoreFormData(ctx, point.Run)
	if err != nil {
		s.serverError(w, err)
		return
	}
	for k, v := range form {
		data[k] = v
	}
	data["Point"], data["Step"], data["SelectedVMID"] = point, 4, vmid
	if strings.HasPrefix(srcKey, "sql:") {
		data["SelectedDB"] = srcKey[strings.LastIndex(srcKey, ":")+1:]
	}
	data["Image"] = imageDetails(point.Run)
	data["Back"] = "/restore?" + url.Values{"type": {typ.Key}, "src": {srcKey}, "run": {strconv.FormatInt(runID, 10)}}.Encode()
	s.render(w, r, "restore", page)
}
