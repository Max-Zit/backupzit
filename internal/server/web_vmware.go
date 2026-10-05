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
)

// handleVMwareCreate adds an ESXi host: the connection is checked first and
// the host's certificate and SSH key are pinned from then on.
func (s *Server) handleVMwareCreate(w http.ResponseWriter, r *http.Request, _ string) {
	h := VMwareHost{
		Name:     strings.TrimSpace(r.FormValue("name")),
		Address:  strings.TrimSpace(r.FormValue("address")),
		Username: strings.TrimSpace(r.FormValue("username")),
		Password: r.FormValue("password"),
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
		s.store.saveVMwareInventory(r.Context(), id, nil, "", h.APIWrites, false, "", "SSH: "+sshErr.Error())
	}
	s.audit(r, "vmware.create", "%s (%s), certificate %s", h.Name, h.Address, h.Thumbprint)
	msg := fmt.Sprintf("ESXi host %s added (ESXi %s). Its certificate %s is pinned.", h.Name, h.Version, h.Thumbprint)
	if sshErr != nil {
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
	if err := s.store.RefreshVMwareHost(r.Context(), h); err != nil {
		redirectErr(w, r, "/agents#vmware", fmt.Errorf("%s: %v", h.Name, err))
		return
	}
	redirectMsg(w, r, "/agents#vmware", "VM list of "+h.Name+" refreshed.")
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
		for _, h := range hosts {
			if t, ok := vmwareTried.Load(h.ID); ok && now.Sub(t.(time.Time)) < 10*time.Minute {
				continue
			}
			if h.InventoryAt == nil || now.Sub(*h.InventoryAt) >= 10*time.Minute {
				vmwareTried.Store(h.ID, now)
				s.RefreshVMwareHost(ctx, h)
			}
		}
	}()
}
