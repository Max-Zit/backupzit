package main

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"
	"unicode"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/localipc"
)

// Enrolling from the menu bar app. A .pkg cannot ask for the enrollment
// code, so the app does: right after the installation (once per login while
// the Mac is not enrolled) and from the menu item "Enroll this Mac…". It
// runs the agent's own "enroll" as root through macOS's administrator
// prompt (osascript "with administrator privileges"): the app itself never
// gets root, and the code reaches the shell only as a quoted argument.

// agentProgram is the agent the package installs.
const agentProgram = "/Library/BackupZit/bin/backupzit-agent"

const (
	enrollWelcome = "BackupZit is installed. To back up this Mac, enroll it with your BackupZit console."
	enrollPrompt  = "Paste the enrollment code from the console (Agents → Create enrollment token; it starts with BZ1-). macOS then asks for an administrator's name and password."
	enrollAuth    = "BackupZit wants to enroll this Mac with your backup console."
	enrollButton  = "Enroll"
)

// askCodeScript asks for the code; it returns the text typed, or nothing
// for "Later" and a dialog left unanswered for 15 minutes.
const askCodeScript = `on run argv
set r to display dialog (item 1 of argv) default answer "" with title "BackupZit" buttons {"Later", "` + enrollButton + `"} default button "` + enrollButton + `" with icon note giving up after 900
if gave up of r then return ""
if button returned of r is not "` + enrollButton + `" then return ""
return text returned of r
end run`

// enrollScript runs the agent's enroll as root after macOS's administrator
// prompt. The code is item 1 of argv, passed to the shell with "quoted
// form of", never pasted into the script or the command line text.
const enrollScript = `on run argv
do shell script "` + agentProgram + ` enroll --code " & quoted form of (item 1 of argv) & " 2>&1" with prompt (item 2 of argv) with administrator privileges
end run`

// infoScript shows a message with an OK button.
const infoScript = `on run argv
display dialog (item 1 of argv) with title "BackupZit" buttons {"OK"} default button "OK" with icon note giving up after 900
end run`

// askCode shows the code dialog; ok is false when the user chose Later.
func askCode(ctx context.Context, prompt string) (string, bool) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", askCodeScript, "--", prompt).Output() // #nosec G204 -- fixed program and script; the text is an argument of the script
	if err != nil {
		return "", false
	}
	code := cleanCode(string(out))
	return code, code != ""
}

// cleanCode drops the spaces and line breaks a copied code may carry.
func cleanCode(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// errEnrollCanceled: the administrator prompt was canceled.
var errEnrollCanceled = errors.New("canceled")

// runEnroll enrolls the Mac with code.
func runEnroll(ctx context.Context, code string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", enrollScript, "--", code, enrollAuth) // #nosec G204 -- fixed program and script; the code is an argument of the script, quoted for the shell there
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg, canceled := osascriptError(stderr.String())
		if canceled {
			return errEnrollCanceled
		}
		if msg == "" {
			msg = err.Error()
		}
		return errors.New(msg)
	}
	return nil
}

// osascriptError reads osascript's error output, e.g.
// "0:153: execution error: agent is already enrolled (…) (1)": the message
// of the command, and whether the user canceled the prompt (-128).
func osascriptError(stderr string) (string, bool) {
	s := strings.TrimSpace(stderr)
	if strings.HasSuffix(s, "(-128)") {
		return "", true
	}
	if i := strings.Index(s, "execution error: "); i >= 0 {
		s = s[i+len("execution error: "):]
	}
	if i := strings.LastIndex(s, " ("); i >= 0 && strings.HasSuffix(s, ")") {
		s = s[:i]
	}
	// The agent writes "error: …"; several lines become one.
	s = strings.Join(strings.Fields(strings.TrimPrefix(s, "error: ")), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s, false
}

// showInfo shows a message.
func showInfo(ctx context.Context, text string) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	_ = exec.CommandContext(ctx, "/usr/bin/osascript", "-e", infoScript, "--", text).Run() // #nosec G204 -- fixed program and script; the text is an argument of the script
}

// sentence makes an error text start with a capital letter and end with a
// full stop.
func sentence(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	s = string(r)
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}

// shouldAskEnroll tells whether the app asks for the code by itself: the
// agent answers, is not enrolled, and holds no code it is still trying
// (an installation with a code while the console was not reachable).
func shouldAskEnroll(st *localipc.Status) bool {
	return st != nil && !st.Enrolled && (st.ServerURL == "" || st.LastError != "")
}

// startEnroll starts the enrollment dialogs unless they are open; auto is
// the prompt the app shows by itself after the installation.
func (t *tray) startEnroll(auto bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.enrolling {
		return
	}
	t.enrolling = true
	go t.enrollFlow(auto)
}

// enrollFlow asks for the code and enrolls the Mac (started by
// startEnroll, which set t.enrolling).
func (t *tray) enrollFlow(auto bool) {
	defer func() {
		t.mu.Lock()
		t.enrolling = false
		t.mu.Unlock()
	}()

	ctx := context.Background()
	prompt := enrollPrompt
	if auto {
		prompt = enrollWelcome + "\n\n" + enrollPrompt
	}
	for {
		code, ok := askCode(ctx, prompt)
		if !ok {
			return
		}
		server, _, _, err := api.DecodeEnrollCode(code)
		if err != nil {
			prompt = sentence(err.Error()) + "\n\n" + enrollPrompt
			continue
		}
		if err := runEnroll(ctx, code); err != nil {
			if errors.Is(err, errEnrollCanceled) {
				return
			}
			prompt = "Enrolling with " + server + " failed: " + sentence(err.Error()) + " Check that this Mac reaches the console's agent address (usually port 8443), or create a new code.\n\n" + enrollPrompt
			continue
		}
		t.refresh()
		showInfo(ctx, "This Mac is now enrolled with "+server+". It appears on the console's Agents page; add it to a backup job there. Then grant the permissions this menu lists, if any are missing.")
		return
	}
}
