//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/winenc"
)

// --- Boot repair -------------------------------------------------------------

// runBootRepair shows the boot repair dialog: choose a Windows
// installation, what to repair, and repair it.
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
			labels = append(labels, tf("Disk %d, Partition %d (%s):  %s %s  Build %s", "Disk %d, partition %d (%s):  %s %s  Build %s",
				w.Disk, w.Partition, strings.ToUpper(w.Style), w.Product, w.Version, w.Build))
		}
		if len(labels) == 0 {
			labels = []string{t("(keine Windows-Installation gefunden – ist das Laufwerk mit BitLocker gesperrt?)", "(no Windows installation found – is the drive locked with BitLocker?)")}
		}
		_ = list.SetModel(labels)
		_ = list.SetCurrentIndex(0)
	}
	err := dcl.Dialog{
		AssignTo:   &dlg,
		Title:      t("Boot reparieren", "Repair boot"),
		MinSize:    dcl.Size{Width: 820, Height: 640},
		Background: solid(colBg),
		Font:       baseFont,
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: dialogFrame(t("Boot reparieren", "Repair boot"), bootText(),
			[]dcl.Widget{
				dcl.Label{AssignTo: &info, TextColor: colMuted},
				section(t("Windows-Installation", "Windows installation")),
				dcl.ListBox{AssignTo: &list, MinSize: dcl.Size{Height: 120}},
				row(
					FlatButton{Text: t("Neu suchen", "Search again"), OnClicked: refresh},
					FlatButton{Text: t("BitLocker-Laufwerk entsperren…", "Unlock a BitLocker drive…"), OnClicked: func() {
						if unlockBitLocker(dlg) {
							refresh()
						}
					}},
				),
				dcl.VSpacer{Size: 4},
				section(t("Reparatur", "What to repair")),
				row(
					dcl.Label{Text: t("Bootdateien für:", "Boot files for:")},
					dcl.ComboBox{AssignTo: &files, Model: []string{t("diesen Rechner (automatisch)", "this computer (automatic)"), "UEFI", "BIOS", t("beides", "both")}, CurrentIndex: 0},
				),
				dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &drivers, Text: t("Disk-Controller-Treiber beim Start laden (nach Wechsel des Controllers)", "Load disk controller drivers at start (after changing the controller)"), Checked: true},
				dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &active, Text: t("Systempartition aktiv setzen (nur MBR)", "Mark the system partition active (MBR only)"), Checked: true},
				dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &mbr, Text: t("Windows-Bootcode in den MBR schreiben (nur MBR)", "Write the Windows boot code to the MBR (MBR only)")},
				muted(t("Vorher werden der Anfang der Disk und der BCD gesichert (Ordner \\backupzit auf dem Stick oder der Backup-Disk, nie auf der reparierten Disk).", "The start of the disk and the BCD are backed up first (folder \\backupzit on the stick or the backup disk, never on the disk being repaired).")),
			},
			[]dcl.Widget{
				dcl.HSpacer{},
				FlatButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
				FlatButton{Text: t("Reparieren", "Repair"), Kind: btnPrimary, OnClicked: func() {
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
			}),
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	// The dialog was just created, so its label takes the text.
	_ = info.SetText(tf("Dieser Rechner startet im Modus %s.", "This computer starts in %s.", firmwareText(imaging.DetectFirmware().String())))
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
		names = append(names, tf("Disk %d, Partition %d, %s", "Disk %d, partition %d, %s", v.Disk, v.Partition, humanBytes(v.Length)))
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
// \backupzit on a writable drive that is not on that disk (the stick or the
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

// writableDir tells whether a file can be created in dir (a CD or a
// write-protected stick cannot take the boot data).
func writableDir(dir string) bool {
	f, err := os.CreateTemp(dir, ".bz-write-test-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// --- Drivers -----------------------------------------------------------------

// runDrivers shows the devices without a driver and loads drivers into
// Windows PE or adds them to an installed Windows.
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
	err := dcl.Dialog{
		AssignTo:   &dlg,
		Title:      t("Treiber", "Drivers"),
		MinSize:    dcl.Size{Width: 820, Height: 520},
		Background: solid(colBg),
		Font:       baseFont,
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: dialogFrame(t("Treiber", "Drivers"), driversText(),
			[]dcl.Widget{
				section(t("Geräte ohne Treiber", "Devices without a driver")),
				dcl.ListBox{AssignTo: &list},
				muted(t("Fehlt die Systemdisk (z. B. NVMe hinter Intel RST/VMD), den Speichertreiber des Herstellers von einem USB-Stick laden.",
					"If the system disk is missing (e.g. NVMe behind Intel RST/VMD), load the vendor's storage driver from a USB stick.")),
			},
			[]dcl.Widget{
				FlatButton{Text: t("Treiber jetzt laden (Ordner)…", "Load drivers now (folder)…"), Kind: btnPrimary, OnClicked: func() {
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
				FlatButton{Text: t("Treiber in ein Windows einspielen…", "Add drivers to a Windows…"), OnClicked: func() { addDriversToWindows(dlg) }},
				FlatButton{Text: t("Neu prüfen", "Check again"), OnClicked: refresh},
				dcl.HSpacer{},
				FlatButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
			}),
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	dlg.Starting().Attach(refresh)
	dlg.Run()
}

// addDriversToWindows adds the drivers of a folder to an installed
// Windows (offline, with DISM), e.g. before it starts on new hardware.
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
		names = append(names, tf("Disk %d, Partition %d: %s %s", "Disk %d, partition %d: %s %s", w.Disk, w.Partition, w.Product, w.Version))
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

// runNetwork shows the network dialog for the adapters Windows PE sees.
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
		// Only fails when the dialog is gone; there is nothing to show then.
		_ = state.SetText(strings.ReplaceAll(text, "\n", "\r\n"))
	}
	err := dcl.Dialog{
		AssignTo:   &dlg,
		Title:      t("Netzwerk", "Network"),
		MinSize:    dcl.Size{Width: 760, Height: 780},
		Background: solid(colBg),
		Font:       baseFont,
		Layout:     dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: dialogFrame(t("Netzwerk", "Network"), t("Adresse der Netzwerkkarte einstellen", "Set the address of the network adapter"),
			[]dcl.Widget{
				dcl.Label{Text: hint, Visible: hint != "", TextColor: colDanger},
				dcl.Composite{Layout: dcl.Grid{Columns: 2, MarginsZero: true, Spacing: 8}, Children: []dcl.Widget{
					dcl.Label{Text: t("Netzwerkkarte:", "Network adapter:")},
					dcl.ComboBox{AssignTo: &adapter, Model: names, CurrentIndex: first},
					dcl.Label{Text: t("Adresse:", "Address:")},
					dcl.ComboBox{AssignTo: &mode, Model: []string{t("automatisch (DHCP)", "automatic (DHCP)"), t("fest", "static")}, CurrentIndex: 0},
					dcl.Label{Text: "IP:"},
					dcl.LineEdit{AssignTo: &ip, CueBanner: "192.168.1.50"},
					dcl.Label{Text: t("Subnetzmaske:", "Subnet mask:")},
					dcl.LineEdit{AssignTo: &mask, Text: "255.255.255.0"},
					dcl.Label{Text: "Gateway:"},
					dcl.LineEdit{AssignTo: &gw, CueBanner: "192.168.1.1"},
					dcl.Label{Text: "DNS:"},
					dcl.LineEdit{AssignTo: &dns, CueBanner: "192.168.1.1"},
				}},
				row(
					FlatButton{Text: t("Übernehmen", "Apply"), Kind: btnPrimary, OnClicked: func() {
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
					FlatButton{Text: t("Netzwerk neu starten", "Restart the network"), OnClicked: func() {
						_ = busy(dlg, t("Netzwerk wird gestartet…", "Starting the network…"), func(context.Context) error {
							_ = exec.Command("wpeutil.exe", "InitializeNetwork").Run() // #nosec G204 -- fixed command
							return nil
						})
						showState()
					}},
				),
				section(t("Aktueller Stand (ipconfig)", "Current state (ipconfig)")),
				dcl.TextEdit{AssignTo: &state, ReadOnly: true, VScroll: true, Font: dcl.Font{Family: "Consolas", PointSize: 9}},
			},
			[]dcl.Widget{
				dcl.HSpacer{},
				FlatButton{Text: t("Schließen", "Close"), OnClicked: func() { dlg.Cancel() }},
			}),
	}.Create(owner)
	if err != nil {
		return err
	}
	showState()
	dlg.Run()
	return nil
}
