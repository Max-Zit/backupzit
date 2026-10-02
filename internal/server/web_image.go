package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/imaging"
	"github.com/backupzit/backupzit/internal/repo"
)

// inventories maps agent ID to its reported disks, for the disk pickers.
func inventories(agents []Agent) map[string][]imaging.Disk {
	out := map[string][]imaging.Disk{}
	for _, a := range agents {
		if d := a.Disks(); len(d) > 0 {
			out[strconv.FormatInt(a.ID, 10)] = d
		}
	}
	return out
}

// partitionSelection reads ticked partitions. Ticking every partition of
// the disk means "whole disk" (nil), so partitions added later are
// included too.
func (s *Server) partitionSelection(r *http.Request, agentID int64, disk int) []int {
	var sel []int
	for _, v := range r.Form["image_parts"] {
		if n, err := strconv.Atoi(v); err == nil {
			sel = append(sel, n)
		}
	}
	if len(sel) == 0 {
		return nil
	}
	if a, err := s.store.GetAgent(r.Context(), agentID); err == nil {
		for _, d := range a.Disks() {
			if d.Number == disk && len(d.Partitions) == len(sel) {
				return nil
			}
		}
	}
	return sel
}

// imageDetails decodes the disk layout reported by an image backup run.
func imageDetails(run Run) *repo.DiskImage {
	if len(run.Details) == 0 {
		return nil
	}
	var img repo.DiskImage
	if json.Unmarshal(run.Details, &img) != nil {
		return nil
	}
	return &img
}

func (s *Server) handleImageRestore(w http.ResponseWriter, r *http.Request, runID int64, back string) {
	if r.FormValue("confirm_erase") != "on" {
		redirectErr(w, r, back, errors.New("confirm that the target disk will be erased"))
		return
	}
	disk, err := strconv.Atoi(r.FormValue("target_disk"))
	if err != nil {
		redirectErr(w, r, back, errors.New("choose a target disk"))
		return
	}
	rid, err := s.store.QueueImageRestore(r.Context(), runID, formID(r, "agent_id"), disk, r.FormValue("keep_offline") == "on")
	if err != nil {
		redirectErr(w, r, back, err)
		return
	}
	s.audit(r, "restore.image", "run #%d from backup #%d to disk %d", rid, runID, disk)
	redirectMsg(w, r, fmt.Sprintf("/runs/%d", rid), "Image restore queued.")
}

// atoiDefault parses a non-negative form number; anything else is 0.
func atoiDefault(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// s3Location builds s3://endpoint/bucket/prefix[?tls=false] from form fields.
func s3Location(endpoint, bucket, prefix string, plainHTTP bool) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	endpoint = strings.TrimRight(endpoint, "/")
	bucket = strings.Trim(strings.TrimSpace(bucket), "/")
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if endpoint == "" || bucket == "" {
		return "", errors.New("S3 endpoint and bucket are required")
	}
	if strings.ContainsAny(endpoint, "/?#") || strings.ContainsAny(bucket, "/?#") {
		return "", errors.New("invalid S3 endpoint or bucket name")
	}
	loc := "s3://" + endpoint + "/" + bucket
	if prefix != "" {
		loc += "/" + prefix
	}
	if plainHTTP {
		loc += "?tls=false"
	}
	return loc, nil
}

// hardenedLocation builds hardened://host:port/path from form fields.
func hardenedLocation(host, dir string) (string, error) {
	host = strings.TrimSpace(host)
	host = strings.TrimPrefix(strings.TrimPrefix(host, "https://"), "hardened://")
	host = strings.TrimRight(host, "/")
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if host == "" || strings.ContainsAny(host, "/?#@ ") {
		return "", errors.New("hardened repository server (host or host:port) is required")
	}
	if dir == "" {
		dir = "backupzit"
	}
	if !hardenedPath.MatchString(dir) {
		return "", errors.New("folder may contain only letters, digits, '.', '_', '-' and '/'")
	}
	if _, _, err := net.SplitHostPort(host); err != nil {
		host = net.JoinHostPort(host, "8500")
	}
	return "hardened://" + host + "/" + dir, nil
}

var hardenedPath = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]*(/[A-Za-z0-9_-][A-Za-z0-9._-]*)*$`)

// smbLocation builds smb://user@host/share/path from form fields.
func smbLocation(host, share, dir, user string) (string, error) {
	host = strings.Trim(strings.TrimSpace(host), `\/`)
	share = strings.Trim(strings.TrimSpace(share), `\/`)
	dir = strings.Trim(strings.ReplaceAll(strings.TrimSpace(dir), `\`, "/"), "/")
	user = strings.TrimSpace(user)
	if host == "" || share == "" || user == "" {
		return "", errors.New("SMB server, share and user are required")
	}
	if strings.ContainsAny(host+share, `/\?#@`) {
		return "", errors.New("invalid SMB server or share name")
	}
	u := url.URL{Scheme: "smb", User: url.User(user), Host: host, Path: "/" + share}
	if dir != "" {
		u.Path += "/" + dir
	}
	return u.String(), nil
}

var azureName = regexp.MustCompile(`^[a-z0-9]{3,24}$`)
var azureContainer = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9]|-[a-z0-9]){2,62}$`)

// azureLocation builds azure://account/container/folder[?endpoint=...].
func azureLocation(account, container, dir, endpoint string) (string, error) {
	account = strings.ToLower(strings.TrimSpace(account))
	container = strings.ToLower(strings.TrimSpace(container))
	dir = strings.Trim(strings.ReplaceAll(strings.TrimSpace(dir), `\`, "/"), "/")
	if !azureName.MatchString(account) {
		return "", errors.New("storage account names have 3-24 lowercase letters and digits")
	}
	if !azureContainer.MatchString(container) {
		return "", errors.New("container names have 3-63 lowercase letters, digits and single hyphens")
	}
	loc := "azure://" + account + "/" + container
	if dir != "" {
		loc += "/" + dir
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		eu, err := url.Parse(endpoint)
		if err != nil || (eu.Scheme != "https" && eu.Scheme != "http") || eu.Host == "" {
			return "", errors.New("the endpoint must be a URL such as https://account.blob.core.usgovcloudapi.net")
		}
		loc += "?" + url.Values{"endpoint": {strings.TrimRight(endpoint, "/")}}.Encode()
	}
	return loc, nil
}

var usbLabel = regexp.MustCompile(`^[A-Za-z0-9_*?-]{1,32}$`)

// usbLocation builds usb://LABEL/folder for rotating removable disks.
func usbLocation(label, dir string) (string, error) {
	label = strings.ToUpper(strings.TrimSpace(label))
	if !usbLabel.MatchString(label) || strings.Trim(label, "*?") == "" {
		return "", errors.New("enter the disks' volume label, e.g. BZBACKUP* for BZBACKUP1, BZBACKUP2 … (letters, digits, - and _; * and ? as wildcards)")
	}
	dir = strings.Trim(strings.ReplaceAll(strings.TrimSpace(dir), `\`, "/"), "/")
	if dir == "" {
		dir = "BackupZit"
	}
	if !hardenedPath.MatchString(dir) {
		return "", errors.New("folder may contain only letters, digits, '.', '_', '-' and '/'")
	}
	return "usb://" + label + "/" + dir, nil
}
