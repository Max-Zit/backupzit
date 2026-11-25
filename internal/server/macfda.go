package server

import (
	"context"
	"fmt"
)

// agentFullDiskAccess records the Full Disk Access a macOS agent reports with
// its polls (0.61, #62). The Agents page marks a Mac without it. When it goes
// from granted to denied (an update of the unsigned package makes macOS ask
// again, or someone switched it off), the administrators get one email and
// the audit log one entry; nothing repeats per poll or per run.
func (s *Server) agentFullDiskAccess(ctx context.Context, a Agent, state, ip string) {
	switch state {
	case "granted", "denied", "unknown":
	default:
		return
	}
	prev, changed, err := s.store.setFullDiskAccess(ctx, a.ID, state)
	if err != nil {
		s.log.Error("store the Full Disk Access of an agent", "agent", a.Hostname, "err", err)
		return
	}
	if !changed || prev != "granted" || state != "denied" {
		return
	}
	name := a.Name()
	s.log.Warn("a Mac agent lost Full Disk Access", "agent", name)
	if _, err := s.store.AppendAudit(ctx, "", "agent.full_disk_access", fmt.Sprintf("agent %s no longer has Full Disk Access", name), ip); err != nil {
		s.log.Error("audit log", "err", err)
	}
	go s.mailAdmins(context.WithoutCancel(ctx), func(l *language) (string, string) {
		return "[BackupZit] " + l.T("Full Disk Access missing on %s", name),
			l.T("The BackupZit agent on the Mac %s no longer has Full Disk Access. Until it has it again, backups skip the folders macOS protects (Mail, Messages, Safari, Desktop, Documents, other apps' data).", name) + "\n\n" +
				l.T("This usually happens after the agent updated itself: macOS asks again because the package is not signed with an Apple Developer ID yet.") + " " +
				l.T("The user of the Mac turns BackupZit Agent on again in System Settings → Privacy & Security → Full Disk Access; the menu bar app shows the steps.") + "\n"
	})
}
