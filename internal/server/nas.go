package server

import (
	"context"
	"errors"
	"net/url"
	"path"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
)

// NAS jobs back up a network share (SMB or NFS) through an agent on any
// machine that reaches it; nothing is installed on the NAS. The agent
// mounts the share for each run. Backups are ordinary file backups.

const ctxNASPassword = "jobs.nas_password"

// checkNASShare validates a share address: smb://host/share[/folder] or
// nfs://host/export.
func checkNASShare(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("enter the share as smb://host/share or nfs://host/export")
	}
	if !validAddress(u.Host) {
		return errors.New("invalid host name in the share address")
	}
	if strings.Trim(u.Path, "/") == "" {
		return errors.New("the share name or export path is missing, e.g. smb://nas01/data")
	}
	switch u.Scheme {
	case "smb", "nfs":
	default:
		return errors.New("the share address starts with smb:// or nfs://")
	}
	return nil
}

// cleanNASFolders normalises the folders of a NAS job: relative to the
// share, slash separated, without "..".
func cleanNASFolders(in []string) ([]string, error) {
	out := []string{}
	for _, p := range in {
		p = strings.Trim(strings.ReplaceAll(strings.TrimSpace(p), `\`, "/"), "/")
		if p == "" {
			continue
		}
		for _, part := range strings.Split(p, "/") {
			if part == ".." || strings.ContainsRune(part, 0) {
				return nil, errors.New("folders are paths inside the share, without ..")
			}
		}
		out = append(out, path.Clean(p))
	}
	return out, nil
}

// addNAS attaches the share of a NAS job to its backups and to restores
// into the original location.
func (s *Server) addNAS(ctx context.Context, run *Run, ar *api.Run) error {
	if run.JobID == nil {
		return nil
	}
	j, err := s.store.GetJob(ctx, *run.JobID)
	if err != nil || j.Kind != JobNAS {
		return nil
	}
	o := j.Options
	pw := o.NASPassword
	if s.store.box != nil {
		if pw, err = s.store.box.open(ctxNASPassword, pw); err != nil {
			return err
		}
	}
	ar.NAS = &api.NASShare{URL: o.NASURL, User: o.NASUser, Domain: o.NASDomain, Password: pw}
	return nil
}

// NASText describes the share of a NAS job for the console, e.g.
// `\\nas01\data` or `nas01:/volume1/data`.
func (o JobOptions) NASText() string {
	u, err := url.Parse(o.NASURL)
	if err != nil {
		return o.NASURL
	}
	if u.Scheme == "nfs" {
		return u.Host + ":" + u.Path
	}
	return `\\` + u.Host + strings.ReplaceAll(strings.TrimRight(u.Path, "/"), "/", `\`)
}
