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

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"golang.org/x/sys/windows"

	"github.com/max-zit/backupzit/internal/imaging"
)

var version = "dev"

var procGetUserDefaultUILanguage = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetUserDefaultUILanguage")

func systemLang() string {
	if procGetUserDefaultUILanguage.Find() == nil {
		id, _, _ := procGetUserDefaultUILanguage.Call()
		if id&0x3FF == 0x07 { // LANG_GERMAN
			return "de"
		}
	}
	return "en"
}

// app is the main window.
type app struct {
	mw     *walk.MainWindow
	status *walk.Label
}

func main() {
	a := &app{}
	tile := func(title, text string, onClick func()) Widget {
		return Composite{
			Layout: VBox{MarginsZero: true, Spacing: 4},
			Children: []Widget{
				PushButton{Text: title, Font: Font{PointSize: 12, Bold: true}, MinSize: Size{Width: 260, Height: 56}, OnClicked: onClick},
				Label{Text: text},
			},
		}
	}
	err := MainWindow{
		AssignTo: &a.mw,
		Title:    "BackupZit Recovery " + version,
		MinSize:  Size{Width: 820, Height: 560},
		Size:     Size{Width: 1024, Height: 700},
		Layout:   VBox{Margins: Margins{Left: 16, Top: 16, Right: 16, Bottom: 8}},
		Children: []Widget{
			Label{Text: "BackupZit Recovery", Font: Font{PointSize: 18, Bold: true}},
			Label{Text: t("Wählen Sie, was Sie tun möchten.", "Choose what you want to do.")},
			VSpacer{Size: 8},
			Composite{
				Layout: Grid{Columns: 2, Spacing: 12},
				Children: []Widget{
					tile(t("Image wiederherstellen", "Restore an image"),
						t("Ganze Disk oder einzelne Partitionen aus einem Backup zurückschreiben", "Write a whole disk or single partitions back from a backup"),
						func() { runRestoreWizard(a.mw) }),
					tile(t("Boot reparieren", "Repair boot"),
						t("Ein vorhandenes Windows wieder startfähig machen", "Make an existing Windows start again"),
						func() { runBootRepair(a.mw) }),
					tile(t("Treiber", "Drivers"),
						t("Treiber laden, wenn Disks oder Netzwerk fehlen", "Load drivers when disks or network are missing"),
						func() { runDrivers(a.mw) }),
					tile(t("Mit Konsole verbinden", "Connect to the console"),
						t("Wiederherstellung aus der BackupZit-Konsole steuern", "Run the restore from the BackupZit console"),
						func() { startConsoleMode(a.mw) }),
				},
			},
			VSpacer{},
			Composite{
				Layout: HBox{MarginsZero: true},
				Children: []Widget{
					PushButton{Text: t("Netzwerk…", "Network…"), OnClicked: func() { runNetwork(a.mw); a.refreshStatus() }},
					PushButton{Text: t("Eingabeaufforderung", "Command prompt"), OnClicked: func() { startDetached("cmd.exe", "/k", "cd /d %SystemDrive%\\") }},
					PushButton{Text: "Notepad", OnClicked: func() { startDetached("notepad.exe") }},
					HSpacer{},
					PushButton{Text: t("Neu starten", "Restart"), OnClicked: func() { powerOff(a.mw, true) }},
					PushButton{Text: t("Herunterfahren", "Shut down"), OnClicked: func() { powerOff(a.mw, false) }},
				},
			},
			Label{AssignTo: &a.status},
		},
	}.Create()
	if err != nil {
		fmt.Fprintln(os.Stderr, "create window:", err)
		os.Exit(1)
	}
	if inWinPE() {
		win.ShowWindow(a.mw.Handle(), win.SW_MAXIMIZE)
	}
	a.refreshStatus()
	// --open=restore|boot|drivers|network opens a window right away (also
	// for startnet.cmd and for screenshots in tests).
	for _, arg := range os.Args[1:] {
		switch strings.TrimPrefix(arg, "--open=") {
		case "restore":
			a.mw.Starting().Attach(func() { runRestoreWizard(a.mw) })
		case "boot":
			a.mw.Starting().Attach(func() { runBootRepair(a.mw) })
		case "drivers":
			a.mw.Starting().Attach(func() { runDrivers(a.mw) })
		case "network":
			a.mw.Starting().Attach(func() { runNetwork(a.mw) })
		}
	}
	go func() {
		for range time.Tick(15 * time.Second) {
			a.mw.Synchronize(a.refreshStatus)
		}
	}()
	a.mw.Run()
}

// refreshStatus shows firmware, network, disks and keyboard.
func (a *app) refreshStatus() {
	fw := imaging.DetectFirmware()
	fwText := fw.String()
	if lang == "de" {
		fwText = strings.NewReplacer("Secure Boot on", "Secure Boot an", "unknown", "unbekannt").Replace(fwText)
	}
	parts := []string{"Firmware: " + fwText}
	if ip := firstIPv4(); ip != "" {
		parts = append(parts, t("Netzwerk: ", "Network: ")+ip)
	} else {
		parts = append(parts, t("Netzwerk: keine Adresse", "Network: no address"))
	}
	if disks, err := imaging.ListDisks(); err == nil {
		parts = append(parts, tf("%d Datenträger", "%d disks", len(disks)))
	} else {
		parts = append(parts, t("Datenträger: ", "Disks: ")+err.Error())
	}
	parts = append(parts, t("Sprache: Deutsch", "Language: English"))
	a.status.SetText(strings.Join(parts, "   ·   "))
}

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

func startDetached(name string, args ...string) {
	cmd := exec.Command(name, args...) // #nosec G204 -- fixed programs started by the user
	cmd.SysProcAttr = &windows.SysProcAttr{CreationFlags: windows.CREATE_NEW_CONSOLE}
	_ = cmd.Start()
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
