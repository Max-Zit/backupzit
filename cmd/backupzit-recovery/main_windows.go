//go:build windows

// Command backupzit-recovery is the graphical program of the BackupZit
// recovery environment (Windows PE): image restore without the console,
// boot repair and drivers. The work is done by internal/recovery and
// internal/imaging; this is only the user interface.
//
// It needs comctl32 version 6: backupzit-recovery.exe.manifest must lie
// next to the program (build-winpe.ps1 copies both).
package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/imaging"
)

var version = "dev"

// systemLang is the display language of Windows (PE), e.g. "de-DE", or ""
// when it cannot be read. In Windows PE it is the language the medium was
// built with (build-winpe.ps1 -Language, set by DISM /Set-AllIntl).
func systemLang() string {
	l, err := windows.GetUserPreferredUILanguages(windows.MUI_LANGUAGE_NAME)
	if err != nil || len(l) == 0 {
		return ""
	}
	return l[0]
}

// app is the main window.
type app struct {
	mw     *walk.MainWindow
	status *statusChips
	// The tiles and the network button, which get the focus back after a
	// window opened at the start (--open=...) closes.
	restoreTile, bootTile, driversTile *walk.CustomWidget
	netBtn                             *walk.PushButton
}

// restoreText describes the restore. These texts are under the tiles of
// the main window and in the header of their dialogs.
func restoreText() string {
	return t("Ganze Disk oder einzelne Partitionen aus einem Backup zurückschreiben", "Write a whole disk or single partitions back from a backup")
}

// bootText describes the boot repair.
func bootText() string {
	return t("Ein vorhandenes Windows wieder startfähig machen", "Make an existing Windows start again")
}

// driversText describes the driver dialog.
func driversText() string {
	return t("Treiber laden, wenn Disks oder Netzwerk fehlen", "Load drivers when disks or network are missing")
}

// newApp creates the main window.
func newApp() (*app, error) {
	a := &app{}
	err := dcl.MainWindow{
		AssignTo:   &a.mw,
		Title:      "BackupZit Recovery " + version,
		MinSize:    minWindow,
		Size:       fitScreen(dcl.Size{Width: 1024, Height: 700}, minWindow),
		Background: solid(colBg),
		Font:       baseFont,
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []dcl.Widget{
			header("BackupZit Recovery", t("Wählen Sie, was Sie tun möchten.", "Choose what you want to do."), nil, nil),
			dcl.Composite{
				StretchFactor: stretchFill,
				Layout:        dcl.VBox{Margins: dcl.Margins{Left: 24, Top: 24, Right: 24, Bottom: 16}, Spacing: 16},
				Children: []dcl.Widget{
					dcl.Composite{
						Layout: dcl.Grid{Columns: 2, Spacing: 16, MarginsZero: true},
						Children: []dcl.Widget{
							Tile{AssignTo: &a.restoreTile, Title: t("Image wiederherstellen", "Restore an image"), Text: restoreText(), Icon: iconDisk,
								OnClicked: func() { runRestoreWizard(a.mw) }},
							Tile{AssignTo: &a.bootTile, Title: t("Boot reparieren", "Repair boot"), Text: bootText(), Icon: iconPower,
								OnClicked: func() { runBootRepair(a.mw) }},
							Tile{AssignTo: &a.driversTile, Title: t("Treiber", "Drivers"), Text: driversText(), Icon: iconChip,
								OnClicked: func() { runDrivers(a.mw) }},
							Tile{Title: t("Mit Konsole verbinden", "Connect to the console"), Text: t("Wiederherstellung aus der BackupZit-Konsole steuern", "Run the restore from the BackupZit console"), Icon: iconScreen,
								OnClicked: func() { startConsoleMode(a.mw) }},
						},
					},
					dcl.VSpacer{},
					StatusChips{AssignTo: &a.status},
				},
			},
			footer(
				FlatButton{Button: &a.netBtn, Text: t("Netzwerk…", "Network…"), OnClicked: func() { runNetwork(a.mw); a.refreshStatus() }},
				FlatButton{Text: t("Eingabeaufforderung", "Command prompt"), OnClicked: func() { startDetached("cmd.exe", "/k", "cd /d %SystemDrive%\\") }},
				FlatButton{Text: "Notepad", OnClicked: func() { startDetached("notepad.exe") }},
				dcl.HSpacer{},
				FlatButton{Text: t("Neu starten", "Restart"), OnClicked: func() { powerOff(a.mw, true) }},
				FlatButton{Text: t("Herunterfahren", "Shut down"), OnClicked: func() { powerOff(a.mw, false) }},
			),
		},
	}.Create()
	if err != nil {
		return nil, err
	}
	a.refreshStatus()
	return a, nil
}

// main shows the main window with the four tasks and the tools below.
func main() {
	a, err := newApp()
	if err != nil {
		fmt.Fprintln(os.Stderr, "create window:", err)
		os.Exit(1)
	}
	if inWinPE() {
		win.ShowWindow(a.mw.Handle(), win.SW_MAXIMIZE)
	}
	// Tab scrolls a view that scrolls (small screens, many partitions) to the
	// focused control.
	revealFocus()
	// --open=restore|boot|drivers|network opens a window right away (also
	// for startnet.cmd and for screenshots in tests).
	for _, arg := range os.Args[1:] {
		switch strings.TrimPrefix(arg, "--open=") {
		case "restore":
			a.mw.Starting().Attach(func() { runRestoreWizard(a.mw); keepFocus(a.restoreTile) })
		case "boot":
			a.mw.Starting().Attach(func() { runBootRepair(a.mw); keepFocus(a.bootTile) })
		case "drivers":
			a.mw.Starting().Attach(func() { runDrivers(a.mw); keepFocus(a.driversTile) })
		case "network":
			a.mw.Starting().Attach(func() { runNetwork(a.mw); a.refreshStatus(); keepFocus(a.netBtn) })
		}
	}
	go func() {
		for range time.Tick(15 * time.Second) {
			a.mw.Synchronize(a.refreshStatus)
		}
	}()
	a.mw.Run()
}

// refreshStatus shows firmware, network, disks and language.
func (a *app) refreshStatus() { a.status.Set(statusChipsNow()) }

// statusChipsNow are the status chips: green when a thing is ready,
// orange when it is missing, red on an error.
func statusChipsNow() []chip {
	chips := []chip{{"Firmware: " + firmwareText(imaging.DetectFirmware().String()), colAccent}}
	if ip := firstIPv4(); ip != "" {
		chips = append(chips, chip{t("Netzwerk: ", "Network: ") + ip, colOK})
	} else {
		chips = append(chips, chip{t("Netzwerk: keine Adresse", "Network: no address"), colWarn})
	}
	switch disks, err := imaging.ListDisks(); {
	case err != nil:
		chips = append(chips, chip{t("Datenträger: ", "Disks: ") + err.Error(), colDanger})
	case len(disks) == 0:
		chips = append(chips, chip{tf("Datenträger: %d", "Disks: %d", 0), colWarn})
	default:
		chips = append(chips, chip{tf("Datenträger: %d", "Disks: %d", len(disks)), colOK})
	}
	return append(chips, chip{t("Sprache: Deutsch", "Language: English"), colMuted})
}

// firstIPv4 is the first IPv4 address of a network adapter that is up,
// or "" when there is none (no driver, no DHCP yet).
func firstIPv4() string {
	ifs, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, ad := range addrs {
			if ipn, ok := ad.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLinkLocalUnicast() {
				return ipn.IP.String()
			}
		}
	}
	return ""
}

// inWinPE tells whether this runs in Windows PE (system drive X:).
func inWinPE() bool { return strings.EqualFold(os.Getenv("SystemDrive"), "X:") }

// startDetached starts a program in a console window of its own and does
// not wait for it.
func startDetached(name string, args ...string) {
	// Not exec.Cmd: it always passes standard handles (NUL when none are
	// set), so a program in a new console wrote to NUL and read an empty
	// stdin: the agent's window stayed empty and cmd.exe closed at once.
	// Without STARTF_USESTDHANDLES the program uses its new console.
	exe, err := exec.LookPath(name)
	if err != nil {
		return
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(append([]string{exe}, args...)))
	if err != nil {
		return
	}
	app, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return
	}
	si := windows.StartupInfo{Cb: uint32(unsafe.Sizeof(windows.StartupInfo{}))}
	var pi windows.ProcessInformation
	if err := windows.CreateProcess(app, line, nil, nil, false, windows.CREATE_NEW_CONSOLE|windows.CREATE_UNICODE_ENVIRONMENT, nil, nil, &si, &pi); err != nil {
		return
	}
	_ = windows.CloseHandle(pi.Thread)
	_ = windows.CloseHandle(pi.Process)
}

// startConsoleMode opens the console mode of the agent in its own window.
func startConsoleMode(owner walk.Form) {
	exe := "backupzit-agent.exe"
	if _, err := exec.LookPath(exe); err != nil {
		walk.MsgBox(owner, "BackupZit", t("backupzit-agent.exe wurde nicht gefunden.", "backupzit-agent.exe was not found."), walk.MsgBoxIconError)
		return
	}
	startDetached("cmd.exe", "/k", exe, "recovery")
}

// powerOff restarts or shuts down the computer after asking.
func powerOff(owner walk.Form, restart bool) {
	q := t("Den Computer jetzt herunterfahren?", "Shut the computer down now?")
	if restart {
		q = t("Den Computer jetzt neu starten?", "Restart the computer now?")
	}
	if walk.MsgBox(owner, "BackupZit", q, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
		return
	}
	if inWinPE() {
		arg := "shutdown"
		if restart {
			arg = "reboot"
		}
		_ = exec.Command("wpeutil.exe", arg).Run() // #nosec G204 -- fixed arguments
		return
	}
	arg := "/s"
	if restart {
		arg = "/r"
	}
	_ = exec.Command("shutdown.exe", arg, "/t", "0").Run() // #nosec G204 -- fixed arguments
}

// errorBox shows an error.
func errorBox(owner walk.Form, err error) {
	walk.MsgBox(owner, "BackupZit", err.Error(), walk.MsgBoxIconError)
}
