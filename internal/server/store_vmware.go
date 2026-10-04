package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
	"github.com/max-zit/backupzit/internal/vmware"
)

// VMwareHost is an ESXi host whose VMs are backed up through a proxy agent.
type VMwareHost struct {
	ID         int64
	Name       string
	Address    string
	Username   string
	Password   string
	Thumbprint string
	SSHHostKey string
	// ProxyAgentID is the agent that reads and writes the VMs' disks.
	ProxyAgentID *int64
	ProxyName    *string
	RepoDir      string
	Inventory    json.RawMessage
	InventoryAt  *time.Time
	Version      string
	APIWrites    bool
	SSHOK        bool
	LastError    string
	CreatedAt    time.Time
}

// Inv decodes the host's VM list.
func (h VMwareHost) Inv() *pve.Inventory {
	if len(h.Inventory) == 0 {
		return nil
	}
	var inv pve.Inventory
	if json.Unmarshal(h.Inventory, &inv) != nil {
		return nil
	}
	return &inv
}

// Mode describes how the host is used.
func (h VMwareHost) Mode() string {
	if h.APIWrites {
		return "vSphere API"
	}
	return "free license: snapshots and restores over SSH"
}

func (h VMwareHost) conn() vmware.Conn {
	return vmware.Conn{Host: h.Address, User: h.Username, Password: h.Password, Thumbprint: h.Thumbprint, SSHHostKey: h.SSHHostKey}
}

func (h VMwareHost) apiHost() *api.VMwareHost {
	return &api.VMwareHost{Name: h.Name, Address: h.Address, User: h.Username, Password: h.Password, Thumbprint: h.Thumbprint, SSHHostKey: h.SSHHostKey}
}

const ctxVMware = "vmware_hosts.password"

const vmwareCols = `h.id, h.name, h.address, h.username, h.password, h.thumbprint, h.ssh_host_key, h.proxy_agent_id, a.hostname, h.repo_dir,
	h.inventory, h.inventory_at, h.version, h.api_writes, h.ssh_ok, h.last_error, h.created_at`

const vmwareFrom = ` FROM vmware_hosts h LEFT JOIN agents a ON a.id=h.proxy_agent_id`

func (s *Store) scanVMwareHost(r pgx.Row) (VMwareHost, error) {
	var h VMwareHost
	err := r.Scan(&h.ID, &h.Name, &h.Address, &h.Username, &h.Password, &h.Thumbprint, &h.SSHHostKey, &h.ProxyAgentID, &h.ProxyName, &h.RepoDir,
		&h.Inventory, &h.InventoryAt, &h.Version, &h.APIWrites, &h.SSHOK, &h.LastError, &h.CreatedAt)
	if err == nil && s.box != nil {
		h.Password, err = s.box.open(ctxVMware, h.Password)
	}
	return h, err
}

func (s *Store) ListVMwareHosts(ctx context.Context) ([]VMwareHost, error) {
	rows, err := s.db.Query(ctx, `SELECT `+vmwareCols+vmwareFrom+` ORDER BY h.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VMwareHost
	for rows.Next() {
		h, err := s.scanVMwareHost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

func (s *Store) GetVMwareHost(ctx context.Context, id int64) (VMwareHost, error) {
	h, err := s.scanVMwareHost(s.db.QueryRow(ctx, `SELECT `+vmwareCols+vmwareFrom+` WHERE h.id=$1`, id))
	return h, notFound(err)
}

func (s *Store) seal(ctxName, v string) string {
	if s.box == nil {
		return v
	}
	return s.box.seal(ctxName, v)
}

// validAddress accepts a host name or IP address, optionally with a port.
func validAddress(a string) bool {
	if a == "" || len(a) > 255 || strings.ContainsAny(a, "/\\@ \t") {
		return false
	}
	host := a
	if h, _, err := net.SplitHostPort(a); err == nil {
		host = h
	}
	return host != ""
}

// CreateVMwareHost stores a host after its connection was checked.
func (s *Store) CreateVMwareHost(ctx context.Context, h VMwareHost) (int64, error) {
	var id int64
	err := s.db.QueryRow(ctx, `INSERT INTO vmware_hosts(name, address, username, password, thumbprint, ssh_host_key, proxy_agent_id, repo_dir,
		inventory, inventory_at, version, api_writes, ssh_ok) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,now(),$10,$11,$12) RETURNING id`,
		h.Name, h.Address, h.Username, s.seal(ctxVMware, h.Password), h.Thumbprint, h.SSHHostKey, h.ProxyAgentID, h.RepoDir,
		h.Inventory, h.Version, h.APIWrites, h.SSHOK).Scan(&id)
	if err != nil && strings.Contains(err.Error(), "duplicate key") {
		return 0, fmt.Errorf("a VMware host named %q already exists", h.Name)
	}
	return id, err
}

// UpdateVMwareProxy changes the agent that works for the host.
func (s *Store) UpdateVMwareProxy(ctx context.Context, id, agentID int64) error {
	_, err := s.db.Exec(ctx, `UPDATE vmware_hosts SET proxy_agent_id=$2 WHERE id=$1`, id, agentID)
	if err == nil {
		_, err = s.db.Exec(ctx, `UPDATE jobs SET agent_id=$2 WHERE vmware_host_id=$1`, id, agentID)
	}
	return err
}

func (s *Store) DeleteVMwareHost(ctx context.Context, id int64) error {
	_, err := s.db.Exec(ctx, `DELETE FROM vmware_hosts WHERE id=$1`, id)
	return err
}

func (s *Store) saveVMwareInventory(ctx context.Context, id int64, inv []byte, version string, apiWrites, sshOK bool, sshKey, lastErr string) error {
	_, err := s.db.Exec(ctx, `UPDATE vmware_hosts SET inventory=COALESCE($2, inventory), inventory_at=CASE WHEN $2::jsonb IS NULL THEN inventory_at ELSE now() END,
		version=CASE WHEN $3='' THEN version ELSE $3 END, api_writes=$4, ssh_ok=$5, ssh_host_key=CASE WHEN ssh_host_key='' THEN $6 ELSE ssh_host_key END, last_error=$7 WHERE id=$1`,
		id, inv, version, apiWrites, sshOK, sshKey, clip(lastErr, 500))
	return err
}

// probeVMware connects to a host, checks SSH and reads its inventory.
func probeVMware(ctx context.Context, h VMwareHost) (inv []byte, c *vmware.Client, sshErr, err error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err = vmware.Connect(ctx, h.conn())
	if err != nil {
		return nil, c, nil, err
	}
	defer c.Logout()
	i, err := c.Inventory(ctx, h.Name)
	if err != nil {
		return nil, c, nil, err
	}
	inv, _ = json.Marshal(i)
	sshErr = c.CheckSSH(ctx)
	return inv, c, sshErr, nil
}

// RefreshVMwareHost re-reads the VM list of a host.
func (s *Store) RefreshVMwareHost(ctx context.Context, h VMwareHost) error {
	inv, c, sshErr, err := probeVMware(ctx, h)
	if err != nil {
		s.saveVMwareInventory(ctx, h.ID, nil, "", h.APIWrites, h.SSHOK, "", err.Error())
		return err
	}
	msg := ""
	if sshErr != nil {
		msg = "SSH: " + sshErr.Error()
	}
	return s.saveVMwareInventory(ctx, h.ID, inv, c.Version, c.APIWrites, sshErr == nil, c.SSHHostKey, msg)
}

// vmwareHostOfJob returns the host of a VMware VM job, or nil.
func (s *Store) vmwareHostOfRun(ctx context.Context, runID int64) (*VMwareHost, error) {
	var id *int64
	if err := s.db.QueryRow(ctx, `SELECT vmware_host_id FROM runs WHERE id=$1`, runID).Scan(&id); err != nil || id == nil {
		return nil, err
	}
	h, err := s.GetVMwareHost(ctx, *id)
	if err != nil {
		return nil, errors.New("the VMware host was removed from the console")
	}
	return &h, nil
}

// checkVMwareSelection validates the VMs of a VMware job: "*" or VM numbers.
func checkVMwareSelection(h VMwareHost, vms, exclude []string) error {
	if len(vms) == 0 {
		return errors.New("choose the virtual machines to back up, or all of them")
	}
	ids := vms
	if len(vms) == 1 && vms[0] == "*" {
		ids = exclude
	}
	for _, v := range ids {
		if n, err := strconv.Atoi(v); err != nil || n < 1 {
			return fmt.Errorf("invalid VM %q", v)
		}
	}
	return nil
}

// addVMware gives the proxy agent the connection of the run's ESXi host.
func (s *Server) addVMware(ctx context.Context, run *Run, ar *api.Run) error {
	h, err := s.store.vmwareHostOfRun(ctx, run.ID)
	if err != nil {
		return err
	}
	if h != nil {
		ar.VMware = h.apiHost()
	}
	return nil
}

var badVMName = regexp.MustCompile(`[/\:*?"<>|%\x00-\x1f]`)

// QueueVMwareRestore creates a vm-restore run of a VMware backup on an ESXi
// host, carried out by the host's proxy agent.
func (s *Store) QueueVMwareRestore(ctx context.Context, backupRunID, hostID int64, o api.VMRestore) (int64, error) {
	b, err := s.GetRun(ctx, backupRunID)
	if err != nil {
		return 0, err
	}
	if vmDetails(b) == nil || b.SnapshotID == "" {
		return 0, errors.New("run has no VM backup to restore")
	}
	if b.Expired {
		return 0, errors.New("this backup was removed by the retention policy")
	}
	d := vmDetails(b)
	if d.Platform() != "vmware" {
		return 0, fmt.Errorf("this is a %s backup; restore it on a %s host", platformLabel(d.Platform()), platformLabel(d.Platform()))
	}
	found := false
	for _, g := range d.Guests {
		found = found || g.VMID == o.VMID
	}
	if !found {
		return 0, fmt.Errorf("VM %d is not in this backup", o.VMID)
	}
	h, err := s.GetVMwareHost(ctx, hostID)
	if err != nil {
		return 0, errors.New("unknown VMware host")
	}
	if h.ProxyAgentID == nil {
		return 0, fmt.Errorf("choose a proxy agent for %s first", h.Name)
	}
	if o.NewVMID != 0 && o.NewVMID != -1 {
		return 0, errors.New("VMware VMs are restored as a new VM or in place of the original")
	}
	o.Name = strings.TrimSpace(o.Name)
	if len(o.Name) > 80 || badVMName.MatchString(o.Name) {
		return 0, errors.New(`the VM name has at most 80 characters and none of / \ : * ? " < > | %`)
	}
	if o.Storage != "" {
		ok := false
		if inv := h.Inv(); inv != nil {
			for _, ds := range inv.Storage {
				ok = ok || ds == o.Storage
			}
		}
		if !ok {
			return 0, fmt.Errorf("datastore %q is not available on %s", o.Storage, h.Name)
		}
	}
	opts, _ := json.Marshal(o)
	var id int64
	err = s.db.QueryRow(ctx, `INSERT INTO runs(agent_id, job_id, kind, trigger, repo_url, target_id, snapshot_id, vm_restore, vmware_host_id)
		VALUES($1,$2,'vm-restore','manual',$3,$4,$5,$6,$7) RETURNING id`,
		*h.ProxyAgentID, b.JobID, b.RepoURL, b.TargetID, b.SnapshotID, opts, h.ID).Scan(&id)
	return id, err
}
