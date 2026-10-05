package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/pve"
)

// StartInstantServer starts the NBD disk server of an instant recovery as
// its own systemd unit, so it keeps serving after this run ended and after
// agent restarts. The repository credentials are handed over in a file
// only root can read.
func StartInstantServer(ctx context.Context, rs api.Repository, snapshotID string, srcVMID, vmid int, socket string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(pve.InstantRunDir, 0o700); err != nil {
		return err
	}
	env := map[string]string{
		"BACKUPZIT_REPO": rs.URL, "BACKUPZIT_PASSWORD": rs.Password,
		"BACKUPZIT_SFTP_PASSWORD": rs.SFTPPassword, "BACKUPZIT_SFTP_HOSTKEY": rs.SFTPHostKey,
		"BACKUPZIT_S3_ACCESS_KEY": rs.S3AccessKey, "BACKUPZIT_S3_SECRET_KEY": rs.S3SecretKey, "BACKUPZIT_S3_REGION": rs.S3Region,
		"BACKUPZIT_SMB_PASSWORD": rs.SMBPassword, "BACKUPZIT_SMB_DOMAIN": rs.SMBDomain,
		"BACKUPZIT_HARDENED_KEY": rs.HardenedKey, "BACKUPZIT_HARDENED_FINGERPRINT": rs.HardenedFingerprint,
		"BACKUPZIT_AZURE_KEY": rs.AzureKey, "BACKUPZIT_AZURE_SAS": rs.AzureSAS,
	}
	if rs.SFTPKey != "" {
		kf := filepath.Join(pve.InstantRunDir, fmt.Sprintf("%d.key", vmid))
		if err := os.WriteFile(kf, []byte(rs.SFTPKey), 0o600); err != nil {
			return err
		}
		env["BACKUPZIT_SFTP_KEY"] = kf
	}
	var b strings.Builder
	for k, v := range env {
		if v != "" {
			// systemd EnvironmentFile: quote and escape.
			b.WriteString(k + "=\"" + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", "").Replace(v) + "\"\n")
		}
	}
	envFile := filepath.Join(pve.InstantRunDir, fmt.Sprintf("%d.env", vmid))
	if err := os.WriteFile(envFile, []byte(b.String()), 0o600); err != nil {
		return err
	}
	unit := pve.InstantUnit(vmid)
	exec.CommandContext(ctx, "systemctl", "stop", unit).Run()
	exec.CommandContext(ctx, "systemctl", "reset-failed", unit).Run()
	out, err := exec.CommandContext(ctx, "systemd-run", "--unit", unit, "--description", "BackupZit instant recovery of VM "+strconv.Itoa(vmid),
		"--property=EnvironmentFile="+envFile, "--property=Restart=on-failure",
		exe, "nbd-serve", "--snapshot", snapshotID, "--vmid", strconv.Itoa(srcVMID), "--socket", socket).CombinedOutput()
	if err != nil {
		return fmt.Errorf("systemd-run: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// vmInstant starts, finishes or discards an instant recovery on this
// Proxmox node.
func (a *Agent) vmInstant(ctx context.Context, run api.Run) api.RunResult {
	if run.VMware != nil {
		return a.vmwareInstant(ctx, run)
	}
	if !pve.Available() {
		return failed(fmt.Errorf("instant recovery needs the agent on a Proxmox VE node"))
	}
	o := run.VMRestore
	if o == nil {
		return failed(fmt.Errorf("options missing"))
	}
	logf := func(msg string, kv ...any) { a.log.Info(msg, append([]any{"run", run.ID}, kv...)...) }
	switch run.Kind {
	case api.KindVMInstantFinish:
		notes, err := pve.InstantFinish(ctx, o.InstantVMID, o.Storage, logf)
		if err != nil {
			return failed(err)
		}
		msg := fmt.Sprintf("VM %d now runs from storage %s; the backup is no longer used", o.InstantVMID, o.Storage)
		if len(notes) > 0 {
			msg += ". " + strings.Join(notes, "; ")
		}
		return api.RunResult{Status: api.StatusSuccess, Message: msg}
	case api.KindVMInstantDiscard:
		if err := pve.InstantDiscard(ctx, o.InstantVMID); err != nil {
			return failed(err)
		}
		return api.RunResult{Status: api.StatusSuccess, Message: fmt.Sprintf("VM %d stopped and deleted", o.InstantVMID)}
	}
	r, closeRepo, err := a.openRepo(ctx, run.Repository, false)
	if err != nil {
		return failed(fmt.Errorf("open repository: %w", err))
	}
	sn, err := r.LoadSnapshot(ctx, run.SnapshotID)
	closeRepo()
	if err != nil {
		return failed(err)
	}
	res, err := pve.InstantStart(ctx, sn, pve.InstantOptions{VMID: o.VMID, NewVMID: o.NewVMID, Name: o.Name, Start: true,
		ServeCommand: func(ctx context.Context, vmid int, socket string) error {
			return StartInstantServer(ctx, run.Repository, sn.ID.String(), o.VMID, vmid, socket)
		}, Log: logf})
	if err != nil {
		return failed(err)
	}
	msg := fmt.Sprintf("VM %d (%s) runs from the backup on node %s", res.VMID, res.Name, res.Node)
	if !res.Started {
		msg += " but did not start"
	}
	if len(res.Notes) > 0 {
		msg += ". " + strings.Join(res.Notes, "; ")
	}
	details, _ := json.Marshal(map[string]any{"instant_vmid": res.VMID, "node": res.Node, "name": res.Name, "state": "running", "disks": res.Disks})
	return api.RunResult{Status: api.StatusSuccess, Message: msg, Details: details}
}
