package server

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/repo"
)

// Browsing file backups (files, NAS shares, Linux systems) in the console:
// the console reads the snapshot from the storage, like image browsing.
// Paths inside a snapshot are its components joined with "/" ("C/Users",
// "srv/www"); they are shown and restored in the syntax of the agent that
// made the backup.

// fileEntry is one row of the file browser.
type fileEntry struct {
	Name    string
	Path    string // components joined with "/"
	IsDir   bool
	Size    uint64
	ModTime time.Time
}

func browsableFileRun(run Run) bool {
	return (run.Kind == api.KindBackup || run.Kind == api.KindSystemBackup) && run.SnapshotID != "" && !run.Expired
}

// fileSnapshot opens the snapshot of a file backup run.
func (s *Server) fileSnapshot(ctx context.Context, run Run) (*repo.Repository, *repo.Snapshot, error) {
	if !browsableFileRun(run) {
		return nil, nil, errors.New("run has no file backup to browse")
	}
	if run.TargetID == nil {
		return nil, nil, errors.New("storage target was deleted")
	}
	t, err := s.store.GetTarget(ctx, *run.TargetID)
	if err != nil {
		return nil, nil, errors.New("storage target was deleted")
	}
	if t.SFTPKey != "" {
		return nil, nil, errors.New("browsing storage that uses SSH key authentication is not supported yet")
	}
	r, err := s.cache.repo(ctx, t, run.RepoURL)
	if err != nil {
		return nil, nil, err
	}
	sn, err := r.LoadSnapshot(context.WithoutCancel(ctx), run.SnapshotID)
	return r, sn, err
}

func snapshotComps(p string) []string {
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c != "" && c != "." && c != ".." {
			out = append(out, c)
		}
	}
	return out
}

// lookupNode finds the node at comps below the root tree (nil node = the
// root itself).
func lookupNode(ctx context.Context, r *repo.Repository, root repo.ID, comps []string) (*repo.Tree, *repo.Node, error) {
	t, err := r.LoadTree(ctx, root)
	if err != nil {
		return nil, nil, err
	}
	var n *repo.Node
	for i, c := range comps {
		n = t.Find(c)
		if n == nil {
			return nil, nil, fmt.Errorf("%s is not in the backup", strings.Join(comps[:i+1], "/"))
		}
		if i == len(comps)-1 {
			break
		}
		if n.Type != repo.NodeDir || n.Subtree == nil {
			return nil, nil, fmt.Errorf("%s is not a folder", strings.Join(comps[:i+1], "/"))
		}
		if t, err = r.LoadTree(ctx, *n.Subtree); err != nil {
			return nil, nil, err
		}
	}
	return t, n, nil
}

// listDir returns the entries of the folder at p.
func listDir(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, p string) ([]fileEntry, error) {
	comps := snapshotComps(p)
	t, n, err := lookupNode(ctx, r, sn.Tree, comps)
	if err != nil {
		return nil, err
	}
	if n != nil {
		if n.Type != repo.NodeDir || n.Subtree == nil {
			return nil, fmt.Errorf("%s is not a folder", p)
		}
		if t, err = r.LoadTree(ctx, *n.Subtree); err != nil {
			return nil, err
		}
	}
	out := make([]fileEntry, 0, len(t.Nodes))
	for _, x := range t.Nodes {
		out = append(out, fileEntry{Name: x.Name, Path: strings.Join(append(append([]string{}, comps...), x.Name), "/"),
			IsDir: x.Type == repo.NodeDir, Size: x.Size, ModTime: x.ModTime})
	}
	return out, nil
}

// agentSyntaxPath shows snapshot components as a path of the agent's system.
func agentSyntaxPath(comps []string, windows bool) string {
	if !windows {
		return "/" + strings.Join(comps, "/")
	}
	if len(comps) == 0 {
		return `\`
	}
	if len(comps[0]) == 1 {
		return comps[0] + `:\` + strings.Join(comps[1:], `\`)
	}
	if comps[0] == "UNC" {
		return `\\` + strings.Join(comps[1:], `\`)
	}
	return `\` + strings.Join(comps, `\`)
}

func (s *Server) runIsWindows(ctx context.Context, run Run) bool {
	a, err := s.store.GetAgent(ctx, run.AgentID)
	return err == nil && strings.HasPrefix(strings.ToLower(a.OS), "windows")
}

type fileCrumb struct{ Name, Path string }

func fileCrumbs(comps []string, windows bool) []fileCrumb {
	out := []fileCrumb{{Name: map[bool]string{true: "\\", false: "/"}[windows], Path: ""}}
	for i := range comps {
		name := comps[i]
		if i == 0 && windows && len(name) == 1 {
			name += ":"
		}
		out = append(out, fileCrumb{Name: name, Path: strings.Join(comps[:i+1], "/")})
	}
	return out
}

func (s *Server) fileRunFromPath(r *http.Request) (Run, error) {
	id, err := pathID(r)
	if err != nil {
		return Run{}, ErrNotFound
	}
	return s.store.GetRun(r.Context(), id)
}

func (s *Server) handleFileBrowse(w http.ResponseWriter, r *http.Request, user string) {
	run, err := s.fileRunFromPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	dir := strings.Join(snapshotComps(r.URL.Query().Get("path")), "/")
	win := s.runIsWindows(r.Context(), run)
	data := map[string]any{"Run": run, "Path": dir, "Crumbs": fileCrumbs(snapshotComps(dir), win), "Windows": win, "M365": s.store.isM365Run(r.Context(), run)}
	pd := pageData{Title: fmt.Sprintf("Files of backup #%d", run.ID), Nav: "runs", User: user, Data: data}
	repoR, sn, err := s.fileSnapshot(r.Context(), run)
	if err == nil {
		var entries []fileEntry
		entries, err = listDir(context.WithoutCancel(r.Context()), repoR, sn, dir)
		data["Entries"] = entries
	}
	if errors.Is(err, ErrBrowseLocal) {
		err = errors.New("this backup is on a disk of an agent; the console cannot read it — restore with the paths instead")
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
	s.render(w, r, "filebrowse", pd)
}

func fileBack(run Run, dir string) string {
	return fmt.Sprintf("/runs/%d/files?", run.ID) + url.Values{"path": {dir}}.Encode()
}

// handleFileDownload streams the ticked entries: one file as is, several
// or folders as a ZIP archive.
func (s *Server) handleFileDownload(w http.ResponseWriter, r *http.Request, user string) {
	run, err := s.fileRunFromPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	dir := strings.Join(snapshotComps(r.FormValue("path")), "/")
	sel := r.Form["sel"]
	if len(sel) == 0 {
		redirectErr(w, r, fileBack(run, dir), errors.New("tick the files and folders to download"))
		return
	}
	repoR, sn, err := s.fileSnapshot(r.Context(), run)
	if err != nil {
		redirectErr(w, r, fileBack(run, dir), err)
		return
	}
	ctx := context.WithoutCancel(r.Context())
	s.audit(r, "restore.download", "%d paths from backup #%d", len(sel), run.ID)
	writeFile := func(dst io.Writer, n *repo.Node) error {
		for _, id := range n.Content {
			b, err := repoR.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				return err
			}
			if _, err := dst.Write(b); err != nil {
				return err
			}
		}
		return nil
	}
	if len(sel) == 1 {
		if _, n, err := lookupNode(ctx, repoR, sn.Tree, snapshotComps(sel[0])); err == nil && n != nil && n.Type == repo.NodeSymlink {
			name := n.Name
			if n, err = followLink(ctx, repoR, sn.Tree, snapshotComps(sel[0]), n); err != nil {
				redirectErr(w, r, fileBack(run, dir), err)
				return
			}
			if n.Type == repo.NodeFile {
				cp := *n
				cp.Name = name
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(cp.Name))
				w.Header().Set("Content-Length", strconv.FormatUint(cp.Size, 10))
				writeFile(w, &cp)
				return
			}
		}
		if _, n, err := lookupNode(ctx, repoR, sn.Tree, snapshotComps(sel[0])); err == nil && n != nil && n.Type == repo.NodeFile {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(n.Name))
			w.Header().Set("Content-Length", strconv.FormatUint(n.Size, 10))
			writeFile(w, n)
			return
		}
	}
	name := fmt.Sprintf("backup%d-%s.zip", run.ID, time.Now().Format("20060102-1504"))
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(name))
	zw := zip.NewWriter(w)
	defer zw.Close()
	var add func(rel string, comps []string, n *repo.Node) error
	add = func(rel string, comps []string, n *repo.Node) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		switch n.Type {
		case repo.NodeDir:
			if n.Subtree == nil {
				return nil
			}
			t, err := repoR.LoadTree(ctx, *n.Subtree)
			if err != nil {
				return nil
			}
			if len(t.Nodes) == 0 {
				zw.CreateHeader(&zip.FileHeader{Name: rel + "/", Modified: n.ModTime})
			}
			for i := range t.Nodes {
				if err := add(path.Join(rel, t.Nodes[i].Name), append(append([]string{}, comps...), t.Nodes[i].Name), &t.Nodes[i]); err != nil {
					return err
				}
			}
		case repo.NodeFile:
			zf, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: n.ModTime})
			if err != nil {
				return err
			}
			return writeFile(zf, n)
		case repo.NodeSymlink:
			// A link to a file is stored as that file; links to folders are
			// left out (they could loop).
			if t, err := followLink(ctx, repoR, sn.Tree, comps, n); err == nil && t.Type == repo.NodeFile {
				zf, err := zw.CreateHeader(&zip.FileHeader{Name: rel, Method: zip.Deflate, Modified: t.ModTime})
				if err != nil {
					return err
				}
				return writeFile(zf, t)
			}
		}
		return nil // devices are not part of the archive
	}
	for _, sp := range sel {
		_, n, err := lookupNode(ctx, repoR, sn.Tree, snapshotComps(sp))
		if err != nil || n == nil {
			continue
		}
		if add(n.Name, snapshotComps(sp), n) != nil {
			return
		}
	}
}

// handleFilePickRestore restores the ticked entries on an agent, into a
// folder or to their original location.
func (s *Server) handleFilePickRestore(w http.ResponseWriter, r *http.Request, _ string) {
	run, err := s.fileRunFromPath(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.ParseForm()
	dir := strings.Join(snapshotComps(r.FormValue("path")), "/")
	back := fileBack(run, dir)
	sel := r.Form["sel"]
	if len(sel) == 0 {
		redirectErr(w, r, back, errors.New("tick the files and folders to restore"))
		return
	}
	target := strings.TrimSpace(r.FormValue("target"))
	if r.FormValue("mode") == "original" {
		target = ""
	} else if target == "" {
		redirectErr(w, r, back, errors.New("enter a folder to restore into, or choose original location"))
		return
	}
	win := s.runIsWindows(r.Context(), run)
	includes := make([]string, 0, len(sel))
	for _, p := range sel {
		if c := snapshotComps(p); len(c) > 0 {
			includes = append(includes, agentSyntaxPath(c, win))
		}
	}
	rid, err := s.store.QueueRestore(r.Context(), run.ID, formID(r, "agent_id"), target, includes, r.FormValue("verify") == "on")
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.files", "run #%d from backup #%d to %q: %d ticked paths", rid, run.ID, target, len(includes))
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Restore queued.")
}

// followLink resolves a symbolic link of a file backup inside the
// snapshot: an absolute target (/srv/data/x, C:\Data\x) or one relative to
// the link's folder. Links to links are followed, up to a limit.
func followLink(ctx context.Context, r *repo.Repository, root repo.ID, comps []string, n *repo.Node) (*repo.Node, error) {
	for i := 0; n != nil && n.Type == repo.NodeSymlink; i++ {
		if i == 16 {
			return nil, fmt.Errorf("%s: too many levels of symbolic links", strings.Join(comps, "/"))
		}
		t := strings.ReplaceAll(n.LinkTarget, `\`, "/")
		var next string
		switch {
		case strings.HasPrefix(t, "/"):
			next = t
		case len(t) > 1 && t[1] == ':': // C:/Data/x
			next = t[:1] + t[2:]
		default:
			next = path.Join(strings.Join(comps[:len(comps)-1], "/"), t)
		}
		next = path.Clean("/" + next)
		target := snapshotComps(next)
		_, nn, err := lookupNode(ctx, r, root, target)
		if err != nil || nn == nil {
			return nil, fmt.Errorf("%s is a link to %s, which is not in the backup", strings.Join(comps, "/"), n.LinkTarget)
		}
		n, comps = nn, target
	}
	return n, nil
}
