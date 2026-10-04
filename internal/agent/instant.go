package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/backupzit/backupzit/internal/api"
	"github.com/backupzit/backupzit/internal/pve"
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
