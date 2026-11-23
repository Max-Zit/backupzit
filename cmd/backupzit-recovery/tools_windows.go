//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/winenc"
)

// --- Boot repair -------------------------------------------------------------

func runBootRepair(owner walk.Form) {
	var dlg *walk.Dialog
	var list *walk.ListBox
	var drivers, mbr, active *walk.CheckBox
	var files *walk.ComboBox
	var info *walk.Label
	var installs []imaging.WindowsInstall
	refresh := func() {
		_ = busy(dlg, t("Windows-Installationen werden gesucht…", "Looking for Windows installations…"), func(ctx context.Context) error {
			var err error
			installs, err = imaging.FindWindowsInstalls(ctx)
			return err
		})
		var labels []string
		for _, w := range installs {
			labels = append(labels, fmt.Sprintf("Disk %d, %s %d (%s):  %s %s  Build %s", w.Disk, t("Partition", "partition"), w.Partition, strings.ToUpper(w.Style), w.Product, w.Version, w.Build))
		}
		if len(labels) == 0 {
			labels = []string{t("(keine Windows-Installation gefunden – ist das Laufwerk mit BitLocker gesperrt?)", "(no Windows installation found – is the drive locked with BitLocker?)")}
		}
		_ = list.SetModel(labels)
		_ = list.SetCurrentIndex(0)
	}
	err := Dialog{
		AssignTo: &dlg,
		Title:    t("Boot reparieren", "Repair boot"),
		MinSize:  Size{Width: 760, Height: 460},
		Layout:   VBox{},
		Children: []Widget{
			Label{AssignTo: &info},
			ListBox{AssignTo: &list, MinSize: Size{Height: 140}},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				PushButton{Text: t("Neu suchen", "Search again"), OnClicked: refresh},
				PushButton{Text: t("BitLocker-Laufwerk entsperren…", "Unlock a BitLocker drive…"), OnClicked: func() {
					if unlockBitLocker(dlg) {
						refresh()
					}
				}},
				HSpacer{},
			}},
			GroupBox{Title: t("Reparatur", "Repair"), Layout: VBox{}, Children: []Widget{
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{Text: t("Bootdateien für:", "Boot files for:")},
					ComboBox{AssignTo: &files, Model: []string{t("diesen Rechner (automatisch)", "this computer (automatic)"), "UEFI", "BIOS", t("beides", "both")}, CurrentIndex: 0},
					HSpacer{},
				}},
				CheckBox{AssignTo: &drivers, Text: t("Disk-Controller-Treiber beim Start laden (nach Wechsel des Controllers)", "Load disk controller drivers at start (after changing the controller)"), Checked: true},
				CheckBox{AssignTo: &active, Text: t("Systempartition aktiv setzen (nur MBR)", "Mark the system partition active (MBR only)"), Checked: true},
				CheckBox{AssignTo: &mbr, Text: t("Windows-Bootcode in den MBR schreiben (nur MBR)", "Write the Windows boot code to the MBR (MBR only)")},
			}},
			Label{Text: t("Vorher werden der Anfang der Disk und der BCD gesichert (Ordner \\backupzit auf dem Stick oder der Backup-Disk, nie auf der reparierten Disk).", "The start of the disk and the BCD are backed up first (folder \\backupzit on the stick or the backup disk, never on the disk being repaired).")},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{Text: t("Reparieren", "Repair"), OnClicked: func() {
					i := list.CurrentIndex()
					if i < 0 || i >= len(installs) {
						errorBox(dlg, errors.New(t("Bitte eine Windows-Installation wählen.", "Choose a Windows installation.")))
						return
					}
					opts := imaging.BootRepairOptions{BootDrivers: drivers.Checked(), SetActive: active.Checked(), MBRCode: mbr.Checked(), BackupDir: backupDir(installs[i].Disk)}
					opts.BootFiles = []string{"", "UEFI", "BIOS", "ALL"}[files.CurrentIndex()]
					var rep *imaging.BootReport
					var lines []string
					err := busy(dlg, t("Boot wird repariert…", "Repairing the boot…"), func(ctx context.Context) error {
						var err error
						rep, err = imaging.RepairBoot(ctx, installs[i], opts, func(s string) { lines = append(lines, logText(s)) })
						return err
					})
					if err != nil {
						errorBox(dlg, err)
						return
					}
					msg := t("Bootdateien neu geschrieben.", "Boot files rebuilt.")
					if !rep.Rebuilt {
						msg = t("Die Bootdateien konnten nicht geschrieben werden.", "The boot files could not be written.")
					}
					if len(lines) > 0 {
						msg += "\r\n\r\n" + strings.Join(lines, "\r\n")
					}
					if len(rep.Warnings) > 0 {
						msg += "\r\n\r\n" + t("Hinweise:", "Notes:") + "\r\n- " + strings.Join(rep.Warnings, "\r\n- ")
					}
					walk.MsgBox(dlg, "BackupZit", msg, walk.MsgBoxIconInformation)
				}},
				PushButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	info.SetText(t("Dieser Rechner startet im Modus ", "This computer starts in ") + imaging.DetectFirmware().String() + ".")
	dlg.Starting().Attach(refresh)
	dlg.Run()
}

// unlockBitLocker asks for a locked volume and its recovery key.
func unlockBitLocker(owner walk.Form) bool {
	vols, err := imaging.LockedBitLockerVolumes()
	if err != nil {
		errorBox(owner, err)
		return false
	}
	if len(vols) == 0 {
		walk.MsgBox(owner, "BackupZit", t("Kein gesperrtes BitLocker-Laufwerk gefunden.", "No locked BitLocker drive found."), walk.MsgBoxIconInformation)
		return false
	}
	var names []string
	for _, v := range vols {
		names = append(names, fmt.Sprintf("Disk %d, %s %d, %s", v.Disk, t("Partition", "partition"), v.Partition, humanBytes(v.Length)))
	}
	k, ok := askChoice(owner, "BitLocker", t("Welches Laufwerk entsperren?", "Which drive to unlock?"), names)
	if !ok {
		return false
	}
	key, ok := askText(owner, "BitLocker", t("Wiederherstellungsschlüssel (48 Ziffern):", "Recovery key (48 digits):"), false)
	if !ok {
		return false
	}
	var letter string
	err = busy(owner, t("Laufwerk wird entsperrt…", "Unlocking the drive…"), func(ctx context.Context) error {
		var err error
		letter, err = imaging.UnlockBitLocker(ctx, vols[k], key)
		return err
	})
	if err != nil {
		errorBox(owner, err)
		return false
	}
	walk.MsgBox(owner, "BackupZit", tf("Entsperrt als %s.", "Unlocked as %s.", letter), walk.MsgBoxIconInformation)
	return true
}

// backupDir is where a boot repair of disk saves the old boot data:
// ackupzit on a writable drive that is not on that disk (the stick or the
// backup disk), else the temp folder.
func backupDir(disk int) string {
	avoid := map[string]bool{}
	if disks, err := imaging.ListDisks(); err == nil {
		for _, d := range disks {
			if d.Number != disk {
				continue
			}
			for _, p := range d.Partitions {
				for _, m := range p.MountPoints {
					avoid[strings.ToUpper(m)] = true
				}
			}
		}
	}
	roots := recovery.DriveRoots()
	for i, r := range roots {
		roots[i] = strings.ToUpper(r)
	}
	return chooseBackupDir(roots, avoid, writableDir)
}

// --- Drivers -----------------------------------------------------------------

func runDrivers(owner walk.Form) {
	var dlg *walk.Dialog
	var list *walk.ListBox
	refresh := func() {
		var devs []imaging.ProblemDevice
		err := busy(dlg, t("Geräte werden geprüft…", "Checking devices…"), func(ctx context.Context) error {
			var err error
			devs, err = imaging.ProblemDevices(ctx)
			return err
		})
		var labels []string
		if err != nil {
			labels = []string{err.Error()}
		}
		for _, d := range devs {
			labels = append(labels, fmt.Sprintf("%s  (%s)  %s  –  %s", d.Description, d.Class, problemText(d.Problem), d.InstanceID))
		}
		if err == nil && len(labels) == 0 {
			labels = []string{t("Alle Geräte haben einen funktionierenden Treiber.", "Every device has a working driver.")}
		}
		_ = list.SetModel(labels)
	}
	err := Dialog{
		AssignTo: &dlg,
		Title:    t("Treiber", "Drivers"),
		MinSize:  Size{Width: 760, Height: 420},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: t("Geräte ohne Treiber:", "Devices without a driver:")},
			ListBox{AssignTo: &list},
			Label{Text: t("Fehlt die Systemdisk (z. B. NVMe hinter Intel RST/VMD), den Speichertreiber des Herstellers von einem USB-Stick laden.",
				"If the system disk is missing (e.g. NVMe behind Intel RST/VMD), load the vendor's storage driver from a USB stick.")},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				PushButton{Text: t("Treiber jetzt laden (Ordner)…", "Load drivers now (folder)…"), OnClicked: func() {
					dir := browseFolder(dlg, t("Ordner mit Treibern (.inf)", "Folder with drivers (.inf)"))
					if dir == "" {
						return
					}
					var res *imaging.LoadDriversResult
					err := busy(dlg, t("Treiber werden geladen…", "Loading drivers…"), func(ctx context.Context) error {
						var err error
						res, err = imaging.LoadDrivers(ctx, dir, nil)
						return err
					})
					if err != nil {
						errorBox(dlg, err)
						return
					}
					msg := tf("%d Treiber geladen, %d fehlgeschlagen.", "%d drivers loaded, %d failed.", len(res.Loaded), len(res.Failed))
					if len(res.Failed) > 0 {
						msg += "\r\n\r\n" + strings.Join(res.Failed, "\r\n")
					}
					walk.MsgBox(dlg, "BackupZit", msg, walk.MsgBoxIconInformation)
					refresh()
				}},
				PushButton{Text: t("Treiber in ein Windows einspielen…", "Add drivers to a Windows…"), OnClicked: func() { addDriversToWindows(dlg) }},
				PushButton{Text: t("Neu prüfen", "Check again"), OnClicked: refresh},
				HSpacer{},
				PushButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
			}},
		},
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	dlg.Starting().Attach(refresh)
	dlg.Run()
}

func addDriversToWindows(owner walk.Form) {
	var installs []imaging.WindowsInstall
	_ = busy(owner, t("Windows-Installationen werden gesucht…", "Looking for Windows installations…"), func(ctx context.Context) error {
		var err error
		installs, err = imaging.FindWindowsInstalls(ctx)
		return err
	})
	if len(installs) == 0 {
		errorBox(owner, errors.New(t("Keine Windows-Installation gefunden.", "No Windows installation found.")))
		return
	}
	var names []string
	for _, w := range installs {
		names = append(names, fmt.Sprintf("Disk %d, %d: %s %s", w.Disk, w.Partition, w.Product, w.Version))
	}
	k, ok := askChoice(owner, t("Treiber", "Drivers"), t("In welches Windows?", "Into which Windows?"), names)
	if !ok {
		return
	}
	dir := browseFolder(owner, t("Ordner mit Treibern (.inf)", "Folder with drivers (.inf)"))
	if dir == "" {
		return
	}
	var n int
	err := busy(owner, t("Treiber werden eingespielt…", "Adding drivers…"), func(ctx context.Context) error {
		var err error
		n, err = imaging.AddDriversToWindows(ctx, installs[k], dir)
		return err
	})
	if err != nil {
		errorBox(owner, err)
		return
	}
	walk.MsgBox(owner, "BackupZit", tf("%d Treiber eingespielt.", "%d drivers added.", n), walk.MsgBoxIconInformation)
}

// --- Network -----------------------------------------------------------------

func runNetwork(owner walk.Form) {
	var names []string
	if ifs, err := net.Interfaces(); err == nil {
		for _, i := range ifs {
			if i.Flags&net.FlagLoopback == 0 {
				names = append(names, i.Name)
			}
		}
	}
	if err := networkDialog(owner, names); err != nil {
		errorBox(owner, err)
	}
}

// networkDialog sets up the network adapters named (none when no driver
// is loaded: the dialog then says so and still shows ipconfig).
func networkDialog(owner walk.Form, names []string) error {
	var dlg *walk.Dialog
	var adapter, mode *walk.ComboBox
	var ip, mask, gw, dns *walk.LineEdit
	var state *walk.TextEdit
	first, hint := 0, ""
	if len(names) == 0 {
		first = -1
		hint = t("Keine Netzwerkkarte gefunden. Meist fehlt ihr Treiber: im Hauptfenster unter „Treiber“ laden, dann „Netzwerk neu starten“.",
			"No network adapter found. Usually its driver is missing: load it under “Drivers” in the main window, then “Restart the network”.")
	}
	noAdapter := errors.New(t("Keine Netzwerkkarte gewählt.", "No network adapter chosen."))
	showState := func() {
		out, _ := exec.Command("ipconfig.exe", "/all").CombinedOutput() // #nosec G204 -- fixed command
		text := strings.ReplaceAll(winenc.Text(out), "\r\n", "\n")
		state.SetText(strings.ReplaceAll(text, "\n", "\r\n"))
	}
	err := Dialog{
		AssignTo: &dlg,
		Title:    t("Netzwerk", "Network"),
		MinSize:  Size{Width: 700, Height: 520},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: hint, Visible: hint != "", TextColor: walk.RGB(0xB0, 0x30, 0x20)},
			Composite{Layout: Grid{Columns: 2}, Children: []Widget{
				Label{Text: t("Netzwerkkarte:", "Network adapter:")},
				ComboBox{AssignTo: &adapter, Model: names, CurrentIndex: first},
				Label{Text: t("Adresse:", "Address:")},
				ComboBox{AssignTo: &mode, Model: []string{t("automatisch (DHCP)", "automatic (DHCP)"), t("fest", "static")}, CurrentIndex: 0},
				Label{Text: "IP:"},
				LineEdit{AssignTo: &ip, CueBanner: "192.168.1.50"},
				Label{Text: t("Subnetzmaske:", "Subnet mask:")},
				LineEdit{AssignTo: &mask, Text: "255.255.255.0"},
				Label{Text: "Gateway:"},
				LineEdit{AssignTo: &gw, CueBanner: "192.168.1.1"},
				Label{Text: "DNS:"},
				LineEdit{AssignTo: &dns, CueBanner: "192.168.1.1"},
			}},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				PushButton{Text: t("Übernehmen", "Apply"), OnClicked: func() {
					name := adapter.Text()
					if name == "" {
						errorBox(dlg, noAdapter)
						return
					}
					var cmds [][]string
					if mode.CurrentIndex() == 0 {
						cmds = [][]string{
							{"interface", "ipv4", "set", "address", "name=" + name, "source=dhcp"},
							{"interface", "ipv4", "set", "dnsservers", "name=" + name, "source=dhcp"},
						}
					} else {
						a := []string{"interface", "ipv4", "set", "address", "name=" + name, "static", ip.Text(), mask.Text()}
						if g := strings.TrimSpace(gw.Text()); g != "" {
							a = append(a, g)
						}
						cmds = [][]string{a}
						if d := strings.TrimSpace(dns.Text()); d != "" {
							cmds = append(cmds, []string{"interface", "ipv4", "set", "dnsservers", "name=" + name, "static", d, "primary"})
						}
					}
					for _, c := range cmds {
						if out, err := exec.Command("netsh.exe", c...).CombinedOutput(); err != nil { // #nosec G204 -- arguments are separate values, no shell
							errorBox(dlg, fmt.Errorf("netsh: %v: %s", err, strings.TrimSpace(string(out))))
							return
						}
					}
					showState()
				}},
				PushButton{Text: t("Netzwerk neu starten", "Restart the network"), OnClicked: func() {
					_ = busy(dlg, t("Netzwerk wird gestartet…", "Starting the network…"), func(context.Context) error {
						_ = exec.Command("wpeutil.exe", "InitializeNetwork").Run() // #nosec G204 -- fixed command
						return nil
					})
					showState()
				}},
				HSpacer{},
				PushButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
			}},
			TextEdit{AssignTo: &state, ReadOnly: true, VScroll: true, Font: Font{Family: "Consolas", PointSize: 9}},
		},
	}.Create(owner)
	if err != nil {
		return err
	}
	showState()
	dlg.Run()
	return nil
}
