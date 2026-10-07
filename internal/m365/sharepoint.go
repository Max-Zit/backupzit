package m365

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/max-zit/backupzit/internal/repo"
)

// SharePoint sites are backed up library by library: every document
// library of a site is a drive, read like a OneDrive. Personal sites
// (OneDrive) are left out; they belong to the accounts.

// Site is a SharePoint site.
type Site struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	WebURL      string `json:"webUrl"`
}

// Drive is a document library of a site.
type Drive struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	DriveType string `json:"driveType"`
}

// personal reports a OneDrive (personal site).
func (s Site) personal() bool {
	return strings.Contains(strings.ToLower(s.WebURL), "-my.sharepoint.com/")
}

// Sites lists the SharePoint sites of the tenant, without personal sites.
func (c *Client) Sites(ctx context.Context) ([]Site, error) {
	all, err := listAll[Site](ctx, c, "/sites/getAllSites?$top=500&$select=id,name,displayName,webUrl")
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, s := range all {
		if !s.personal() && s.WebURL != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].WebURL) < strings.ToLower(out[j].WebURL) })
	return out, nil
}

// SiteByURL looks up a site by its address, e.g.
// https://contoso.sharepoint.com/sites/sales.
func (c *Client) SiteByURL(ctx context.Context, raw string) (Site, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return Site{}, fmt.Errorf("%q is not a site address", raw)
	}
	p := "/sites/" + url.PathEscape(u.Host)
	if rel := strings.TrimRight(u.Path, "/"); rel != "" {
		p += ":" + (&url.URL{Path: rel}).EscapedPath() + ":"
	}
	var s Site
	err = c.getJSON(ctx, p+"?$select=id,name,displayName,webUrl", &s)
	return s, err
}

func (c *Client) siteDrives(ctx context.Context, site string) ([]Drive, error) {
	return listAll[Drive](ctx, c, "/sites/"+url.PathEscape(site)+"/drives?$top=100&$select=id,name,driveType")
}

// sharePoint backs up the document libraries of the job's sites (every
// site of the tenant when none are named). It returns the SharePoint
// folder and the addresses of the sites backed up.
func (b *backup) sharePoint(ctx context.Context, ptree *repo.Tree) (repo.Node, []string, error) {
	var sites []Site
	if len(b.opts.Sites) == 0 {
		all, err := b.c.Sites(ctx)
		if err != nil {
			if isDenied(err) {
				return repo.Node{}, nil, fmt.Errorf("list the SharePoint sites: %w (grant the application permission Sites.Read.All with admin consent)", err)
			}
			return repo.Node{}, nil, fmt.Errorf("list the SharePoint sites: %w", err)
		}
		sites = all
	} else {
		for _, raw := range b.opts.Sites {
			s, err := b.c.SiteByURL(ctx, raw)
			switch {
			case err == nil:
				sites = append(sites, s)
			case isDenied(err):
				return repo.Node{}, nil, fmt.Errorf("read the SharePoint site %s: %w (grant the application permission Sites.Read.All with admin consent)", raw, err)
			case IsNotFound(err):
				b.addError(raw, errors.New("no such SharePoint site"))
			default:
				b.addError(raw, err)
			}
		}
	}
	t := &repo.Tree{}
	used := map[string]bool{}
	var done []string
	for _, s := range sites {
		name := uniqueName(used, siteName(s))
		at := SharePointDir + "/" + name
		drives, err := b.c.siteDrives(ctx, s.ID)
		if err != nil {
			if ctx.Err() != nil {
				return repo.Node{}, nil, ctx.Err()
			}
			b.addError(at, err)
			continue
		}
		if len(drives) == 0 {
			continue // e.g. the search center: no document libraries
		}
		sort.Slice(drives, func(i, j int) bool { return drives[i].Name < drives[j].Name })
		pt := b.subtree(ctx, ptree, name)
		st := &repo.Tree{}
		dused := map[string]bool{}
		for _, d := range drives {
			dn := uniqueName(dused, cleanName(d.Name, 120))
			sub, err := b.driveFolder(ctx, d.ID, "root", at+"/"+dn, b.subtree(ctx, pt, dn))
			if err != nil {
				return repo.Node{}, nil, err
			}
			n, err := b.dirNode(ctx, dn, sub)
			if err != nil {
				return repo.Node{}, nil, err
			}
			st.Nodes = append(st.Nodes, n)
		}
		n, err := b.dirNode(ctx, name, st)
		if err != nil {
			return repo.Node{}, nil, err
		}
		t.Nodes = append(t.Nodes, n)
		done = append(done, s.WebURL)
	}
	n, err := b.dirNode(ctx, SharePointDir, t)
	return n, done, err
}

// siteName names a site's folder: its title, or its address path.
func siteName(s Site) string {
	if n := strings.TrimSpace(s.DisplayName); n != "" {
		return cleanName(n, 120)
	}
	if u, err := url.Parse(s.WebURL); err == nil {
		if p := strings.Trim(u.Path, "/"); p != "" {
			return cleanName(strings.ReplaceAll(p, "/", "-"), 120)
		}
		return cleanName(u.Host, 120)
	}
	return cleanName(s.Name, 120)
}
