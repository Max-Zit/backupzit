package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *Server) encryptedTarget(r *http.Request) (Target, error) {
	id, err := pathID(r)
	if err != nil {
		return Target{}, ErrNotFound
	}
	t, err := s.store.GetTarget(r.Context(), id)
	if err != nil {
		return t, err
	}
	if !t.Encrypted {
		return t, errors.New("this storage target is not encrypted")
	}
	return t, nil
}

func (s *Server) handleRecoveryKey(w http.ResponseWriter, r *http.Request, user string) {
	t, err := s.encryptedTarget(r)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	s.log.Warn("recovery key displayed", "target", t.Name, "user", user, "remote", s.guard.ClientIP(r))
	s.audit(r, "storage.recovery_key", "recovery key of %s displayed", t.Name)
	s.render(w, r, "recoverykey", pageData{Title: "Recovery key", Nav: "targets", User: user, Data: t})
}

// recoverySheet is a printable document with everything needed to read
// the backups on a target without this server.
func recoverySheet(t Target, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BackupZit RECOVERY SHEET\r\n========================\r\n\r\n")
	fmt.Fprintf(&b, "Storage target: %s\r\nLocation:       %s\r\nCreated:        %s\r\n\r\n", t.Name, t.URL, now.Format("2006-01-02 15:04"))
	fmt.Fprintf(&b, "RECOVERY KEY:   %s\r\n\r\n", t.RecoveryKey)
	b.WriteString("Backups on this storage are encrypted with AES-256. Without this key\r\n" +
		"they cannot be read by anyone, including BackupZit support.\r\n" +
		"Keep this sheet in a safe place (print it, store it offline).\r\n\r\n" +
		"Each machine has its own repository below the location:\r\n" +
		"  <location>/<hostname>_<id>\r\n\r\n" +
		"Restoring without the BackupZit server, on any machine with the agent\r\n" +
		"(Windows: set NAME=value in an administrator prompt; Linux: export NAME=value as root):\r\n" +
		"  BACKUPZIT_PASSWORD=<recovery key>\r\n")
	// The storage's own credentials are not on the sheet; their names are,
	// together with the fingerprints the console pinned (not secret).
	switch t.Kind {
	case "sftp":
		b.WriteString("  BACKUPZIT_SFTP_PASSWORD=<password of the storage account> (or BACKUPZIT_SFTP_KEY=<private key file>)\r\n")
		if t.SFTPHostKey != "" {
			fmt.Fprintf(&b, "  BACKUPZIT_SFTP_HOSTKEY=%s\r\n", t.SFTPHostKey)
		} else {
			b.WriteString("  BACKUPZIT_SFTP_HOSTKEY=<SSH host key fingerprint of the server, SHA256:...>\r\n")
		}
	case "s3":
		b.WriteString("  BACKUPZIT_S3_ACCESS_KEY=<access key>\r\n  BACKUPZIT_S3_SECRET_KEY=<secret key>\r\n")
		if t.S3Region != "" {
			fmt.Fprintf(&b, "  BACKUPZIT_S3_REGION=%s\r\n", t.S3Region)
		}
	case "smb":
		b.WriteString("  BACKUPZIT_SMB_PASSWORD=<password of the share account>\r\n")
		if t.SMBDomain != "" {
			fmt.Fprintf(&b, "  BACKUPZIT_SMB_DOMAIN=%s\r\n", t.SMBDomain)
		}
	case "hardened":
		b.WriteString("  BACKUPZIT_HARDENED_KEY=<access key of the hardened repository>\r\n")
		fmt.Fprintf(&b, "  BACKUPZIT_HARDENED_FINGERPRINT=%s\r\n", t.HardenedFingerprint)
	case "azure":
		b.WriteString("  BACKUPZIT_AZURE_KEY=<storage account key> (or BACKUPZIT_AZURE_SAS=<SAS token>)\r\n")
	}
	b.WriteString("then:\r\n" +
		"  backupzit-agent snapshots --repo <location>/<hostname>_<id>\r\n" +
		"  backupzit-agent restore --repo <location>/<hostname>_<id> --target C:\\Restore latest\r\n")
	if t.S3LockDays > 0 {
		fmt.Fprintf(&b, "\r\nImmutable storage (S3 Object Lock, %s). If backups were deleted or\r\n"+
			"overwritten, add --as-of <time before the incident>, e.g.\r\n"+
			"  backupzit-agent snapshots --repo <location>/<hostname>_<id> --as-of 2026-10-01T14:30\r\n", days(t.S3LockDays))
	}
	return b.String()
}

func (s *Server) handleRecoverySheet(w http.ResponseWriter, r *http.Request, user string) {
	t, err := s.encryptedTarget(r)
	if err != nil {
		redirectErr(w, r, "/targets", err)
		return
	}
	s.log.Warn("recovery sheet downloaded", "target", t.Name, "user", user, "remote", s.guard.ClientIP(r))
	s.audit(r, "storage.recovery_sheet", "recovery sheet of %s downloaded", t.Name)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="backupzit-recovery-sheet.txt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write([]byte(recoverySheet(t, time.Now())))
}

func days(n int) string {
	if n == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", n)
}
