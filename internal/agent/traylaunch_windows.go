package agent

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// TrayExe is the tray app next to the agent executable.
const TrayExe = "backupzit-tray.exe"

// StartTrayInSessions starts the tray app in every logged-on user session
// that does not run it yet. Logons later start it through the Run key the
// installer sets; this covers installation and updates, which happen while
// users are logged on.
func StartTrayInSessions(log *slog.Logger) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	tray := filepath.Join(filepath.Dir(self), TrayExe)
	if _, err := os.Stat(tray); err != nil {
		return
	}
	running := traySessions()
	var sessions *windows.WTS_SESSION_INFO
	var count uint32
	if err := windows.WTSEnumerateSessions(0, 0, 1, &sessions, &count); err != nil {
		return
	}
	defer windows.WTSFreeMemory(uintptr(unsafe.Pointer(sessions)))
	for _, s := range unsafe.Slice(sessions, count) {
		if s.State != windows.WTSActive && s.State != windows.WTSDisconnected {
			continue
		}
		if s.SessionID == 0 || running[s.SessionID] {
			continue
		}
		if err := startInSession(s.SessionID, tray); err != nil {
			log.Debug("start tray app", "session", s.SessionID, "err", err)
		} else {
			log.Info("tray app started", "session", s.SessionID)
		}
	}
}

// traySessions returns the sessions that already run the tray app.
func traySessions() map[uint32]bool {
	out := map[uint32]bool{}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return out
	}
	defer windows.CloseHandle(snap)
	var pe windows.ProcessEntry32
	pe.Size = uint32(unsafe.Sizeof(pe))
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		if strings.EqualFold(windows.UTF16ToString(pe.ExeFile[:]), TrayExe) {
			var sid uint32
			if windows.ProcessIdToSessionId(pe.ProcessID, &sid) == nil {
				out[sid] = true
			}
		}
	}
	return out
}

func startInSession(session uint32, exe string) error {
	var tok windows.Token
	if err := windows.WTSQueryUserToken(session, &tok); err != nil {
		return err
	}
	defer tok.Close()
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, tok, false); err != nil {
		return err
	}
	defer windows.DestroyEnvironmentBlock(env)
	si := windows.StartupInfo{Desktop: windows.StringToUTF16Ptr(`winsta0\default`)}
	si.Cb = uint32(unsafe.Sizeof(si))
	var pi windows.ProcessInformation
	cmd := windows.StringToUTF16Ptr(`"` + exe + `"`)
	err := windows.CreateProcessAsUser(tok, nil, cmd, nil, nil, false,
		windows.CREATE_UNICODE_ENVIRONMENT|windows.DETACHED_PROCESS, env, windows.StringToUTF16Ptr(filepath.Dir(exe)), &si, &pi)
	if err != nil {
		return err
	}
	windows.CloseHandle(pi.Thread)
	windows.CloseHandle(pi.Process)
	return nil
}
