package server

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/imaging"
)

// Downloads from backups (files, disk images, VM disks) share these rules:
//   - the Restore permission is needed, the same as for restoring on an agent;
//   - every download is in the audit log with the paths that left the backup;
//   - names inside a ZIP are relative and cleaned, whatever the backup holds,
//     so a crafted name cannot write outside the folder it is unpacked to;
//   - an error after the first byte aborts the response, so a cut download
//     fails in the browser instead of looking complete.

// setAttachment sets the headers of a download. The name is encoded as in
// RFC 6266/2231, so Japanese, Cyrillic or German names arrive intact.
func setAttachment(w http.ResponseWriter, name, contentType string, size int64) {
	cd := mime.FormatMediaType("attachment", map[string]string{"filename": name})
	if cd == "" {
		cd = "attachment; filename=download"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", cd)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if size >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	}
}

// zipEntryName turns a path below the browsed folder into a safe ZIP entry
// name: forward slashes, no leading slash, no "." or ".." parts and no
// control characters.
func zipEntryName(rel string) string {
	rel = strings.ReplaceAll(rel, `\`, "/")
	var parts []string
	for _, s := range strings.Split(rel, "/") {
		s = strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return -1
			}
			return r
		}, s)
		switch s {
		case "", ".", "..":
			continue
		}
		parts = append(parts, s)
	}
	if len(parts) == 0 {
		return "file"
	}
	return strings.Join(parts, "/")
}

// auditPaths lists the downloaded paths for the audit log: the first 20
// (each cut to 200 characters), then how many more.
func auditPaths(sel []string) string {
	const maxPaths, maxLen = 20, 200
	var b strings.Builder
	for i, p := range sel {
		if i == maxPaths {
			fmt.Fprintf(&b, " +%d more", len(sel)-maxPaths)
			break
		}
		if i > 0 {
			b.WriteString(", ")
		}
		if r := []rune(p); len(r) > maxLen {
			p = string(r[:maxLen]) + "…"
		}
		b.WriteString(strconv.Quote(p))
	}
	return b.String()
}

// abortDownload ends a response whose body has started: the connection is
// closed without finishing it, so the browser reports a failed download.
func (s *Server) abortDownload(what string, err error) {
	s.log.Warn("download aborted", "what", what, "err", err)
	panic(http.ErrAbortHandler)
}

// handleImageDownload sends ticked files and folders of a disk image
// partition (NTFS) to the browser: one file as is, several as a ZIP.
func (s *Server) handleImageDownload(w http.ResponseWriter, r *http.Request, _ string) {
	id, err := pathID(r)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	part, _ := strconv.Atoi(r.FormValue("part"))
	dir := imaging.CleanPath(r.FormValue("path"))
	back := fmt.Sprintf("/runs/%d/browse?", id) + url.Values{"part": {strconv.Itoa(part)}, "path": {dir}}.Encode()
	run, err := s.store.GetRun(r.Context(), id)
	if errors.Is(err, ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		s.serverError(w, err)
		return
	}
	sel := r.Form["sel"]
	if len(sel) == 0 {
		redirectErr(w, r, back, errors.New("tick the files and folders to download"))
		return
	}
	v, _, err := s.imageVolume(r.Context(), run, part)
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	// Find every ticked entry before anything is sent: a read error must
	// show as a message, not as a broken download.
	entries := make([]imaging.Entry, 0, len(sel))
	for _, sp := range sel {
		e, err := v.Stat(sp)
		if err != nil {
			redirectErr(w, r, back, err)
			return
		}
		entries = append(entries, e)
	}
	s.audit(r, "restore.download", "%d paths from image backup #%d (partition %d): %s", len(sel), run.ID, part, auditPaths(sel))
	if len(entries) == 1 && !entries[0].IsDir {
		rd, size, err := v.ReadFile(entries[0].Path)
		if err != nil {
			redirectErr(w, r, back, err)
			return
		}
		setAttachment(w, entries[0].Name, "application/octet-stream", size)
		if _, err := io.Copy(w, rd); err != nil {
			s.abortDownload("image file", err)
		}
		return
	}
	setAttachment(w, fmt.Sprintf("image%d-%s.zip", run.ID, time.Now().Format("20060102-1504")), "application/zip", -1)
	zw := zip.NewWriter(w)
	// Bounded like the restore walks (P6): a damaged or crafted NTFS can nest
	// folders without end or contain a folder in itself.
	seen := imaging.Visited{}
	var add func(e imaging.Entry, depth int) error
	add = func(e imaging.Entry, depth int) error {
		if err := r.Context().Err(); err != nil {
			return err
		}
		if depth > imaging.MaxDepth {
			return fmt.Errorf("%s: more than %d folders deep (damaged file system?)", e.Path, imaging.MaxDepth)
		}
		rel := zipEntryName(strings.TrimPrefix(e.Path, dir))
		if e.IsDir {
			if !seen.Enter(e.ID) {
				return nil
			}
			children, err := v.List(e.Path)
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
		rd, _, err := v.ReadFile(e.Path)
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
	for _, e := range entries {
		seen = imaging.Visited{}
		if err := add(e, 0); err != nil {
			s.abortDownload("image archive", err)
		}
	}
	if err := zw.Close(); err != nil {
		s.abortDownload("image archive", err)
	}
}
