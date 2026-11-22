package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// trayLabel is the launchd label of the LaunchAgent the .pkg installs to
// open the menu bar app at login.
const trayLabel = "com.backupzit.tray"

// trayLaunchAgent is that LaunchAgent's file.
const trayLaunchAgent = "/Library/LaunchAgents/" + trayLabel + ".plist"

// checkLoginItem reports whether the menu bar app opens at login. launchd
// sets XPC_SERVICE_NAME to the label of the job it starts; otherwise
// "launchctl print" (read-only) tells whether the job is loaded for this
// user, which it is not once switched off in Login Items.
func checkLoginItem(ctx context.Context) permState {
	_, err := os.Stat(trayLaunchAgent)
	exists := err == nil
	started := os.Getenv("XPC_SERVICE_NAME") == trayLabel
	loaded := false
	if exists && !started {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		target := "gui/" + strconv.Itoa(os.Getuid()) + "/" + trayLabel
		loaded = exec.CommandContext(ctx, "/bin/launchctl", "print", target).Run() == nil // #nosec G204 -- fixed program, the target is built from the uid and a constant
	}
	return loginItemState(exists, started, loaded)
}

// dialogScript shows the guidance text (passed as an argument, never
// pasted into the script) with the two answers; an unanswered dialog closes
// after 15 minutes.
const dialogScript = `on run argv
display dialog (item 1 of argv) with title "BackupZit" buttons {"Later", "` + dialogButtonOpen + `"} default button "` + dialogButtonOpen + `" with icon caution giving up after 900
end run`

// showDialog shows a native dialog and reports whether the user chose to
// open System Settings.
func showDialog(ctx context.Context, text string) bool {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/osascript", "-e", dialogScript, "--", text).Output() // #nosec G204 -- fixed program and script; the text is an argument of the script
	if err != nil {
		return false
	}
	return openChosen(string(out))
}

// openSettings opens a System Settings pane.
func openSettings(url string) {
	if !strings.HasPrefix(url, "x-apple.systempreferences:") {
		return
	}
	_ = exec.Command("/usr/bin/open", url).Run() // #nosec G204 -- fixed program; only System Settings URLs
}

// openURL opens the console in the default browser.
func openURL(u string) {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return
	}
	_ = exec.Command("/usr/bin/open", u).Run() // #nosec G204 -- fixed program; only web addresses
}

// appDir is the app's folder in the user's Application Support.
func appDir() (string, error) {
	d, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "BackupZit"), nil
}

// guideFile holds the assistant's memory (guideMemory).
const guideFile = "tray.json"

func loadGuideMemory() guideMemory {
	var m guideMemory
	d, err := appDir()
	if err != nil {
		return m
	}
	b, err := os.ReadFile(filepath.Join(d, guideFile)) // #nosec G304 -- the user's own Application Support folder
	if err != nil {
		return m
	}
	_ = json.Unmarshal(b, &m)
	return m
}

func saveGuideMemory(m guideMemory) error {
	d, err := appDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(d, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp := filepath.Join(d, guideFile+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(d, guideFile))
}

// errRunning means another menu bar app runs for this user.
var errRunning = errors.New("already running")

// lockInstance keeps one menu bar app per user: launchd starts one at
// login, and starting it again by hand must not add a second icon. The
// lock is in the user's own Caches folder and is released when the process
// ends.
func lockInstance() (*os.File, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return nil, err
	}
	d = filepath.Join(d, "BackupZit")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(d, "tray.lock"), os.O_CREATE|os.O_RDWR, 0o600) // #nosec G304 -- the user's own Caches folder
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil { // #nosec G115 -- a file descriptor fits an int
		_ = f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errRunning
		}
		return nil, err
	}
	return f, nil
}
