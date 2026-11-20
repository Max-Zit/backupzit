package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/vmware"
)

// handleVMwareCreate adds an ESXi host: the connection is checked first and
// the host's certificate and SSH key are pinned from then on.
func (s *Server) handleVMwareCreate(w http.ResponseWriter, r *http.Request, _ string) {
	h := VMwareHost{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Address:  strings.TrimSpace(r.FormValue("address")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Password: r.FormValue("password"),
		Method:   vmwareMethod(r.FormValue("method")),
	}
	if h.Name == "" {
		h.Name = h.Address
	}
	if !validAddress(h.Address) || len(h.Name) > 80 || h.Username == "" {
		redirectErr(w, r, "/agents#vmware", errors.New("enter the ESXi host's address, a user and its password"))
		return
	}
	if id := formID(r, "proxy_agent_id"); id > 0 {
		if _, err := s.store.GetAgent(r.Context(), id); err != nil {
			redirectErr(w, r, "/agents#vmware", errors.New("unknown proxy agent"))
			return
		}
		h.ProxyAgentID = &id
	}
	if r.FormValue("via_proxy") == "on" {
		s.createVMwareViaProxy(w, r, h)
		return
	}
	inv, c, sshErr, err := probeVMware(r.Context(), h)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("cannot connect to %s: %v", h.Address, err))
		return
	}
	h.Thumbprint, h.SSHHostKey, h.Version, h.APIWrites, h.SSHOK, h.Inventory = c.Thumbprint, c.SSHHostKey, c.Version, c.APIWrites, sshErr == nil, inv
	b := make([]byte, 4)
	rand.Read(b)
	h.RepoDir = agentRepoDir("esxi-"+h.Name, hex.EncodeToString(b))
	id, err := s.store.CreateVMwareHost(r.Context(), h)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", err)
		return
	}
	if sshErr != nil {
		_ = s.store.saveVMwareInventory(r.Context(), id, nil, "", h.APIWrites, false, "", "SSH: "+sshErr.Error())
	}
	s.audit(r, "vmware.create", "%s (%s), certificate %s", h.Name, h.Address, h.Thumbprint)
	msg := fmt.Sprintf("ESXi host %s added (ESXi %s). Its certificate %s is pinned.", h.Name, h.Version, h.Thumbprint)
	if c.VCenter {
		msg = fmt.Sprintf("vCenter Server %s added (vCenter %s). Its certificate %s is pinned.", h.Name, h.Version, h.Thumbprint)
	}
	if c.Method() == vmware.MethodAPI {
		msg += " It is licensed: backups and restores use the vSphere API, SSH is not needed."
	} else if sshErr != nil {
		msg += " SSH is not reachable: enable it on the host (Host → Actions → Services → Enable Secure Shell) — restores and backups on the free license need it."
	}
	if h.ProxyAgentID == nil {
		msg += " Choose a proxy agent before creating jobs."
	}
	redirectMsg(w, r, "/agents#vmware", msg)
}

func (s *Server) handleVMwareRefresh(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if h.ViaProxy {
		// The proxy reads it (#51); the page shows the result when it is
		// quick, otherwise a moment later.
		l := requestLanguage(r)
		runID, err := s.store.queueVMwareProbe(r.Context(), h, "manual")
		if err != nil && !errors.Is(err, errProbeQueued) {
			redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
			return
		}
		if err == nil {
			if run, done := s.store.waitProbe(r.Context(), runID, 15*time.Second); done {
				if run.Status != api.StatusSuccess {
					redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %s", h.Name, run.Message))
					return
				}
				redirectMsg(w, r, "/agents#vmware", l.T("VM list of %s refreshed through the proxy agent.", h.Name))
				return
			}
		}
		redirectMsg(w, r, "/agents#vmware", l.T("The proxy agent reads the VM list of %s as soon as it is free; reload this page in a minute.", h.Name))
		return
	}
	if err := s.store.RefreshVMwareHost(r.Context(), h); err != nil {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
		return
	}
	redirectMsg(w, r, "/agents#vmware", "VM list of "+h.Name+" refreshed.")
}

// vmwareMethod accepts the methods of the host form.
func vmwareMethod(m string) string {
	if m == vmware.MethodSSH || m == vmware.MethodAPI {
		return m
	}
	return vmware.MethodAuto
}

// handleVMwareMethod serves POST /vmware/{id}/method: how the host is
// changed (automatic, always SSH, always the API); checked at once.
func (s *Server) handleVMwareMethod(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.Method = vmwareMethod(r.FormValue("method"))
	if h.ViaProxy {
		// Checked by the proxy (#51): stored now, its check shows any error.
		if err := s.store.UpdateVMwareMethod(r.Context(), id, h.Method, h.APIWrites, h.SSHOK); err != nil {
			s.serverError(w, err)
			return
		}
		s.audit(r, "vmware.method", "%s: %q (checked through the proxy agent)", h.Name, h.Method)
		_, _ = s.store.queueVMwareProbe(r.Context(), h, "manual")
		redirectMsg(w, r, "/agents#vmware", requestLanguage(r).T("%s: saved; the proxy agent checks the connection now.", h.Name))
		return
	}
	_, c, sshErr, err := probeVMware(r.Context(), h)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
		return
	}
	if err := s.store.UpdateVMwareMethod(r.Context(), id, h.Method, c.APIWrites, sshErr == nil); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.method", "%s: %q, now %s", h.Name, h.Method, c.Method())
	l := requestLanguage(r)
	if sshErr != nil {
		redirectErr(w, r, "/agents#vmware", errors.New(l.T("%s now uses SSH, but SSH is not reachable: %v", h.Name, sshErr)))
		return
	}
	if c.Method() == vmware.MethodAPI {
		redirectMsg(w, r, "/agents#vmware", l.T("%s: backups and restores now use the vSphere API.", h.Name))
		return
	}
	redirectMsg(w, r, "/agents#vmware", l.T("%s: backups and restores now use SSH.", h.Name))
}

// handleVMwareCertificate serves POST /vmware/{id}/certificate: an admin
// accepts the new certificate a host presented (after a reinstall, a
// renewed certificate or when vCenter took the host over). Only the
// fingerprint that was shown is accepted, and it is checked at once.
func (s *Server) handleVMwareCertificate(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	l := requestLanguage(r)
	fp := r.FormValue("thumbprint")
	if fp == "" || fp != h.NewCertificate() {
		redirectErr(w, r, "/agents#vmware", errors.New(l.T("The certificate of %s changed again: refresh it and check the new fingerprint.", h.Name)))
		return
	}
	old := h.Thumbprint
	h.Thumbprint = fp
	if h.ViaProxy {
		// The proxy saw this fingerprint; its next check uses it (#51).
		if err := s.store.UpdateVMwareThumbprint(r.Context(), id, fp); err != nil {
			s.serverError(w, err)
			return
		}
		_, _ = s.store.db.Exec(r.Context(), `UPDATE vmware_hosts SET last_error='' WHERE id=$1`, id)
		s.audit(r, "vmware.certificate", "%s: certificate %s accepted (was %s), seen by the proxy agent", h.Name, fp, old)
		_, _ = s.store.queueVMwareProbe(r.Context(), h, "manual")
		redirectMsg(w, r, "/agents#vmware", l.T("%s: the new certificate %s is pinned.", h.Name, fp))
		return
	}
	if err := s.store.RefreshVMwareHost(r.Context(), h); err != nil {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
		return
	}
	if err := s.store.UpdateVMwareThumbprint(r.Context(), id, fp); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.certificate", "%s: certificate %s accepted (was %s)", h.Name, fp, old)
	redirectMsg(w, r, "/agents#vmware", l.T("%s: the new certificate %s is pinned.", h.Name, fp))
}

func (s *Server) handleVMwareProxy(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	aid := formID(r, "proxy_agent_id")
	a, err := s.store.GetAgent(r.Context(), aid)
	if err != nil {
		redirectErr(w, r, "/agents#vmware", errors.New("unknown agent"))
		return
	}
	if err := s.store.UpdateVMwareProxy(r.Context(), id, aid); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.proxy", "%s via %s", h.Name, a.Hostname)
	if h.ViaProxy {
		h.ProxyAgentID = &aid
		if _, err := s.store.queueVMwareProbe(r.Context(), h, "manual"); err != nil && !errors.Is(err, errProbeQueued) {
			redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s is now backed up through %s, but its VM list cannot be read through it: %v", h.Name, a.Hostname, err))
			return
		}
	}
	redirectMsg(w, r, "/agents#vmware", fmt.Sprintf("%s is now backed up through %s.", h.Name, a.Hostname))
}

func (s *Server) handleVMwareDelete(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.needsApproval(w, r, "/agents#vmware", "vmware.delete", id, h.Name, nil) {
		return
	}
	if err := s.store.DeleteVMwareHost(r.Context(), id); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.delete", "%s (%s)", h.Name, h.Address)
	redirectMsg(w, r, "/agents#vmware", "ESXi host "+h.Name+" and its jobs removed. Its backups remain on the storage targets.")
}

var (
	vmwareRefreshing atomic.Bool
	vmwareTried      sync.Map // host ID → time of the last attempt
)

// refreshVMware re-reads the VM lists of ESXi hosts every 10 minutes.
func (s *Store) refreshVMware(ctx context.Context, now time.Time) {
	if !vmwareRefreshing.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer vmwareRefreshing.Store(false)
		hosts, err := s.ListVMwareHosts(ctx)
		if err != nil {
			return
		}
		s.pruneProbeRuns(ctx)
		for _, h := range hosts {
			if t, ok := vmwareTried.Load(h.ID); ok && now.Sub(t.(time.Time)) < 10*time.Minute {
				continue
			}
			if h.InventoryAt == nil || now.Sub(*h.InventoryAt) >= 10*time.Minute {
				vmwareTried.Store(h.ID, now)
				if h.ViaProxy {
					// The proxy reads it (#51); a busy or offline proxy just
					// leaves the list as it is.
					_, _ = s.queueVMwareProbe(ctx, h, "scheduled")
					continue
				}
				_ = s.RefreshVMwareHost(ctx, h)
			}
		}
	}()
}

// handleVMwareHotAdd serves POST /vmware/{id}/hotadd: hot-add on or off.
// The proxy agent checks itself, at each backup, whether it is a VM of the
// host; if not, or if anything fails, it copies the disks as before.
func (s *Server) handleVMwareHotAdd(w http.ResponseWriter, r *http.Request, _ string) {
	id, _ := pathID(r)
	h, err := s.store.GetVMwareHost(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	on := r.FormValue("hot_add") == "on"
	if err := s.store.UpdateVMwareHotAdd(r.Context(), id, on); err != nil {
		s.serverError(w, err)
		return
	}
	s.audit(r, "vmware.hotadd", "%s: hot-add %v", h.Name, on)
	l := requestLanguage(r)
	if on {
		redirectMsg(w, r, "/agents#vmware", l.T("%s: the proxy agent reads the disks of running VMs through its own VM (hot-add) when it can, otherwise it copies them as before.", h.Name))
		return
	}
	redirectMsg(w, r, "/agents#vmware", l.T("%s: the disks of running VMs are copied on the datastore.", h.Name))
}
