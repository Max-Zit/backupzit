package server

import "net/http"

// docPages is the in-console user guide, in reading order.
var docPages = []struct{ Slug, Title string }{
	{"start", "Getting started"},
	{"install", "Installing the server"},
	{"agents", "Agents"},
	{"storage", "Storage targets"},
	{"jobs", "Backup jobs & schedules"},
	{"restore", "Restoring files"},
	{"images", "Disk images & bare-metal recovery"},
	{"proxmox", "Proxmox VE virtual machines"},
	{"ransomware", "Ransomware protection"},
	{"monitoring", "Monitoring, reports & calendar"},
	{"notifications", "Email notifications"},
	{"users", "Users, roles & LDAP"},
	{"api", "REST API"},
	{"cli", "Command line reference"},
	{"security", "Security & best practices"},
	{"troubleshooting", "Troubleshooting & FAQ"},
}

func (s *Server) handleDocs(w http.ResponseWriter, r *http.Request, user string) {
	page := r.PathValue("page")
	if page == "" {
		page = "start"
	}
	title := ""
	var prev, next any
	for i, p := range docPages {
		if p.Slug == page {
			title = p.Title
			if i > 0 {
				prev = docPages[i-1]
			}
			if i < len(docPages)-1 {
				next = docPages[i+1]
			}
		}
	}
	if title == "" {
		http.NotFound(w, r)
		return
	}
	s.render(w, r, "doc_"+page, pageData{Title: "Docs · " + title, Nav: "docs", User: user,
		Data: map[string]any{"Page": page, "Pages": docPages, "Title": title, "Prev": prev, "Next": next}})
}
