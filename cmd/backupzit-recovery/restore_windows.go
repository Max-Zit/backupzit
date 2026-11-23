//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/recovery"
	"github.com/max-zit/backupzit/internal/repo"
)

// Restore modes on the target page.
const (
	modeWholeDisk = iota
	modeReplace
	modeFree
)

// wizard is the image restore assistant.
type wizard struct {
	dlg   *walk.Dialog
	pages []*walk.Composite
	page  int
	back  *walk.PushButton
	next  *walk.PushButton
	title *walk.Label

	// Source.
	sourceList *walk.ListBox
	sources    []sourceItem
	location   *walk.LineEdit
	password   *walk.LineEdit
	storUser   *walk.LineEdit
	storPass   *walk.LineEdit
	session    *recovery.Session

	// Snapshot.
	snapList  *walk.ListBox
	snaps     []recovery.ImageSnapshot
	imageList *walk.ComboBox
	snapInfo  *walk.TextEdit

	// Target.
	diskList  *walk.ListBox
	disks     []imaging.Disk
	modeBox   *walk.ComboBox
	partBox   *walk.ComboBox
	protected map[int]string

	// Layout.
	styleBox     *walk.ComboBox
	view         *walk.CustomWidget
	rows         *walk.Composite
	layoutErr    *walk.Label
	layout       *imaging.Layout
	limits       map[int]imaging.PartitionLimits
	sizeEdit     *walk.NumberEdit
	singleInfo   *walk.Label
	singleSource int

	// Options.
	newHW      *walk.CheckBox
	driverDir  *walk.LineEdit
	verify     *walk.CheckBox
	keepOff    *walk.CheckBox
	sameDiskHt *walk.Label

	// Summary and progress.
	summary  *walk.TextEdit
	confirm  *walk.LineEdit
	progress *walk.ProgressBar
	phase    *walk.Label
	log      *walk.TextEdit
	running  bool
	cancel   context.CancelFunc
}

// sourceItem is an entry of the source list: a repository found on a
// drive, or an access package.
type sourceItem struct {
	label   string
	repo    string
	pkgPath string
}

func runRestoreWizard(owner walk.Form) {
	w := &wizard{}
	page := func(assign **walk.Composite, children ...Widget) Widget {
		return Composite{AssignTo: assign, Layout: VBox{MarginsZero: true}, Visible: false, Children: children}
	}
	var pSource, pSnap, pTarget, pLayout, pOptions, pSummary, pProgress *walk.Composite
	err := Dialog{
		AssignTo: &w.dlg,
		Title:    t("Image wiederherstellen", "Restore an image"),
		MinSize:  Size{Width: 900, Height: 640},
		Size:     Size{Width: 1000, Height: 720},
		Layout:   VBox{},
		Children: []Widget{
			Label{AssignTo: &w.title, Font: Font{PointSize: 13, Bold: true}},
			page(&pSource,
				Label{Text: t("Gefundene Backups und Zugangspakete auf den Laufwerken:", "Backups and access packages found on the drives:")},
				ListBox{AssignTo: &w.sourceList, MinSize: Size{Height: 120}, MaxSize: Size{Height: 160}, OnCurrentIndexChanged: w.sourceSelected},
				PushButton{Text: t("Laufwerke erneut durchsuchen", "Search the drives again"), OnClicked: w.findSources},
				GroupBox{
					Title:  t("Speicherort", "Location"),
					Layout: Grid{Columns: 2},
					Children: []Widget{
						Label{Text: t("Ort:", "Location:")},
						LineEdit{AssignTo: &w.location, CueBanner: `E:\backups\pc1  ·  smb://user@nas/backups/pc1  ·  sftp://...  ·  s3://...`},
						Label{Text: t("Repo-Passwort oder Wiederherstellungsschlüssel:", "Repository password or recovery key:")},
						LineEdit{AssignTo: &w.password, PasswordMode: true},
						Label{Text: t("Speicher-Benutzer / S3-Zugriffsschlüssel:", "Storage user / S3 access key:")},
						LineEdit{AssignTo: &w.storUser},
						Label{Text: t("Speicher-Passwort / S3-Geheimschlüssel:", "Storage password / S3 secret key:")},
						LineEdit{AssignTo: &w.storPass, PasswordMode: true},
					},
				},
				Label{Text: t("Für SMB den Benutzer in den Ort schreiben (smb://benutzer@server/freigabe) oder oben eintragen. Netzwerk einrichten: im Hauptfenster.",
					"For SMB put the user into the location (smb://user@server/share) or enter it above. Network settings: main window.")},
			),
			page(&pSnap,
				Label{Text: t("Backups mit Disk-Images, das neueste zuerst:", "Backups with disk images, newest first:")},
				ListBox{AssignTo: &w.snapList, MinSize: Size{Height: 160}, MaxSize: Size{Height: 220}, OnCurrentIndexChanged: w.snapSelected},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{Text: t("Gesicherte Disk:", "Backed up disk:")},
					ComboBox{AssignTo: &w.imageList, OnCurrentIndexChanged: w.imageSelected},
					HSpacer{},
				}},
				TextEdit{AssignTo: &w.snapInfo, ReadOnly: true, VScroll: true, Font: Font{Family: "Consolas", PointSize: 9}},
			),
			page(&pTarget,
				Label{Text: t("Zieldatenträger (alles auf der gewählten Disk bzw. Partition wird überschrieben):", "Target disk (everything on the chosen disk or partition is overwritten):")},
				ListBox{AssignTo: &w.diskList, MinSize: Size{Height: 160}, MaxSize: Size{Height: 260}, OnCurrentIndexChanged: w.targetSelected},
				PushButton{Text: t("Datenträger neu einlesen", "Read the disks again"), OnClicked: w.loadDisks},
				Composite{Layout: Grid{Columns: 2, MarginsZero: true}, Children: []Widget{
					Label{Text: t("Wiederherstellen:", "Restore:")},
					ComboBox{AssignTo: &w.modeBox, Model: []string{
						t("Ganze Disk (Zieldisk wird neu angelegt)", "Whole disk (the target disk is recreated)"),
						t("Eine Partition über eine vorhandene Partition", "One partition over an existing partition"),
						t("Eine Partition in freien Platz", "One partition into free space"),
					}, CurrentIndex: 0, OnCurrentIndexChanged: w.modeChanged},
					Label{Text: t("Partition des Ziels:", "Partition of the target:")},
					ComboBox{AssignTo: &w.partBox, Enabled: false},
				}},
			),
			page(&pLayout,
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{Text: t("Partitionstabelle:", "Partition table:")},
					ComboBox{AssignTo: &w.styleBox, Model: []string{t("wie im Backup", "as in the backup"), "GPT (UEFI)", "MBR (BIOS)"}, CurrentIndex: 0, OnCurrentIndexChanged: func() { w.makeLayout(imaging.LayoutFit) }},
					PushButton{Text: t("Wie im Backup", "As in the backup"), OnClicked: func() { w.makeLayout(imaging.LayoutAsSource) }},
					PushButton{Text: t("An Zieldisk anpassen", "Fit to the target disk"), OnClicked: func() { w.makeLayout(imaging.LayoutFit) }},
					HSpacer{},
				}},
				CustomWidget{AssignTo: &w.view, MinSize: Size{Height: 120}, Paint: w.paintLayout, ClearsBackground: true},
				ScrollView{
					Layout:          VBox{MarginsZero: true},
					HorizontalFixed: true,
					MinSize:         Size{Height: 200},
					Children: []Widget{
						Composite{AssignTo: &w.rows, Layout: VBox{MarginsZero: true, Spacing: 2}},
						VSpacer{},
					},
				},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{AssignTo: &w.singleInfo, Visible: false},
					NumberEdit{AssignTo: &w.sizeEdit, Visible: false, Decimals: 2, Suffix: " GiB", MinValue: 0.01, MaxValue: 1 << 20, OnValueChanged: w.singleSizeChanged},
					HSpacer{},
				}},
				Label{AssignTo: &w.layoutErr, TextColor: walk.RGB(0xB0, 0, 0)},
			),
			page(&pOptions,
				CheckBox{AssignTo: &w.newHW, Text: t("Für neue Hardware vorbereiten (anderer Rechner oder Controller: Boot-Treiber, Treiber, Bootdateien)", "Prepare for new hardware (another computer or controller: boot drivers, drivers, boot files)")},
				Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
					Label{Text: t("Zusätzliche Treiber (Ordner):", "Additional drivers (folder):")},
					LineEdit{AssignTo: &w.driverDir},
					PushButton{Text: "…", MaxSize: Size{Width: 30}, OnClicked: func() {
						if d := browseFolder(w.dlg, t("Treiberordner", "Driver folder")); d != "" {
							w.driverDir.SetText(d)
						}
					}},
				}},
				CheckBox{AssignTo: &w.verify, Text: t("Backup vorher vollständig lesen und prüfen (dauert etwa so lang wie die Wiederherstellung)", "Read and check the whole backup first (takes about as long as the restore)")},
				Label{AssignTo: &w.sameDiskHt, TextColor: walk.RGB(0xB0, 0x60, 0), Visible: false,
					Text: t("Die Zieldisk sieht aus wie die gesicherte Disk. Prüfen Sie das Backup vorher: ist es beschädigt, wäre sonst beides verloren.",
						"The target disk looks like the backed up disk. Check the backup first: if it is damaged, both would be lost.")},
				CheckBox{AssignTo: &w.keepOff, Text: t("Zieldisk danach offline lassen (wenn die alte Disk noch im Rechner steckt)", "Leave the target disk offline (when the old disk is still in the computer)")},
				VSpacer{},
			),
			page(&pSummary,
				TextEdit{AssignTo: &w.summary, ReadOnly: true, VScroll: true, MinSize: Size{Height: 300}, Font: Font{Family: "Consolas", PointSize: 9}},
				Label{Text: t("Zum Bestätigen die Nummer der Zieldisk eintippen:", "Type the number of the target disk to confirm:")},
				LineEdit{AssignTo: &w.confirm, MaxSize: Size{Width: 80}},
			),
			page(&pProgress,
				Label{AssignTo: &w.phase},
				ProgressBar{AssignTo: &w.progress, MinValue: 0, MaxValue: 1000},
				TextEdit{AssignTo: &w.log, ReadOnly: true, VScroll: true},
			),
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &w.back, Text: t("< Zurück", "< Back"), OnClicked: func() { w.show(w.page - 1) }},
				PushButton{AssignTo: &w.next, Text: t("Weiter >", "Next >"), OnClicked: w.forward},
				PushButton{Text: t("Schließen", "Close"), OnClicked: w.close},
			}},
		},
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	w.pages = []*walk.Composite{pSource, pSnap, pTarget, pLayout, pOptions, pSummary, pProgress}
	w.dlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if w.running {
			*canceled = true
		}
	})
	w.show(0)
	w.dlg.Starting().Attach(w.findSources)
	w.dlg.Run()
	if w.session != nil {
		_ = w.session.Close()
	}
}

var pageTitles = [][2]string{
	{"1. Backup öffnen", "1. Open the backup"},
	{"2. Backup und Disk wählen", "2. Choose backup and disk"},
	{"3. Ziel wählen", "3. Choose the target"},
	{"4. Partitionen und Größen", "4. Partitions and sizes"},
	{"5. Optionen", "5. Options"},
	{"6. Zusammenfassung", "6. Summary"},
	{"7. Wiederherstellung", "7. Restore"},
}

func (w *wizard) show(i int) {
	if i < 0 || i >= len(w.pages) {
		return
	}
	for k, p := range w.pages {
		p.SetVisible(k == i)
	}
	w.page = i
	w.title.SetText(t(pageTitles[i][0], pageTitles[i][1]))
	w.back.SetEnabled(i > 0 && i < len(w.pages)-1)
	w.next.SetVisible(i < len(w.pages)-1)
	w.next.SetText(t("Weiter >", "Next >"))
	if i == len(w.pages)-2 {
		w.next.SetText(t("Wiederherstellen", "Restore"))
	}
}

func (w *wizard) close() {
	if w.running {
		if walk.MsgBox(w.dlg, "BackupZit", t("Die Wiederherstellung abbrechen? Die Zieldisk ist danach unbrauchbar.", "Cancel the restore? The target disk is unusable afterwards."), walk.MsgBoxYesNo|walk.MsgBoxIconWarning) == 6 && w.cancel != nil {
			w.cancel()
		}
		return
	}
	w.dlg.Cancel()
}

// forward checks the current page and goes to the next one.
func (w *wizard) forward() {
	var err error
	switch w.page {
	case 0:
		err = w.openSource()
	case 1:
		err = w.checkSnapshot()
	case 2:
		err = w.checkTarget()
	case 3:
		err = w.checkLayout()
	case 4:
		err = w.checkOptions()
	case 5:
		err = w.start()
		if err == nil {
			return
		}
	}
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	w.show(w.page + 1)
}

// --- Source ---------------------------------------------------------------

func (w *wizard) findSources() {
	var pkgs []recovery.AccessPackageInfo
	var repos []string
	_ = busy(w.dlg, t("Laufwerke werden nach Backups durchsucht…", "Searching the drives for backups…"), func(context.Context) error {
		roots := recovery.DriveRoots()
		pkgs = recovery.FindAccessPackages(roots)
		repos = recovery.FindLocalRepos(roots, 3)
		return nil
	})
	w.sources = nil
	for _, p := range pkgs {
		w.sources = append(w.sources, sourceItem{label: tf("Zugangspaket %s (%s)", "Access package %s (%s)", p.Path, p.Created.Local().Format("2006-01-02")), pkgPath: p.Path})
	}
	for _, r := range repos {
		w.sources = append(w.sources, sourceItem{label: t("Backup-Speicher ", "Backup storage ") + r, repo: r})
	}
	var labels []string
	for _, s := range w.sources {
		labels = append(labels, s.label)
	}
	if len(labels) == 0 {
		labels = []string{t("(nichts gefunden – Ort unten eintragen)", "(nothing found – enter the location below)")}
	}
	_ = w.sourceList.SetModel(labels)
}

func (w *wizard) sourceSelected() {
	i := w.sourceList.CurrentIndex()
	if i < 0 || i >= len(w.sources) {
		return
	}
	s := w.sources[i]
	if s.repo != "" {
		w.location.SetText(s.repo)
		return
	}
	pass, ok := askText(w.dlg, t("Zugangspaket", "Access package"), t("Passphrase des Zugangspakets:", "Passphrase of the access package:"), true)
	if !ok {
		return
	}
	b, err := os.ReadFile(s.pkgPath)
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	pkg, err := recovery.OpenAccessPackage(b, pass)
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	names := make([]string, len(pkg.Repositories))
	for k, r := range pkg.Repositories {
		names[k] = r.Name + "  –  " + r.Location
	}
	k := 0
	if len(names) > 1 {
		var ok bool
		if k, ok = askChoice(w.dlg, t("Zugangspaket", "Access package"), t("Welches Backup?", "Which backup?"), names); !ok {
			return
		}
	}
	a := pkg.Repositories[k]
	src, cleanup, err := a.Source(os.TempDir())
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	w.openWith(src, cleanup)
}

// openSource opens the repository from the fields (or keeps the one
// opened from an access package).
func (w *wizard) openSource() error {
	loc := strings.TrimSpace(w.location.Text())
	if w.session != nil && w.session.Source.Location == loc {
		return nil
	}
	if loc == "" {
		return errors.New(t("Bitte einen Speicherort angeben oder ein gefundenes Backup wählen.", "Enter a location or choose a backup found."))
	}
	src := recovery.Source{Location: loc, Password: w.password.Text()}
	user, pass := strings.TrimSpace(w.storUser.Text()), w.storPass.Text()
	switch {
	case strings.HasPrefix(loc, "smb://"):
		src.Options.SMBPassword = pass
		if user != "" && !strings.Contains(strings.TrimPrefix(loc, "smb://"), "@") {
			src.Location = "smb://" + user + "@" + strings.TrimPrefix(loc, "smb://")
		}
	case strings.HasPrefix(loc, "sftp://"):
		src.Options.SFTPPassword = pass
	case strings.HasPrefix(loc, "s3://"):
		src.Options.S3AccessKey, src.Options.S3SecretKey = user, pass
	case strings.HasPrefix(loc, "azure://"):
		src.Options.AzureKey = pass
	case strings.HasPrefix(loc, "hardened://"):
		src.Options.HardenedKey = pass
	}
	return w.openWith(src, func() {})
}

func (w *wizard) openWith(src recovery.Source, cleanup func()) error {
	defer cleanup()
	var s *recovery.Session
	var sns []recovery.ImageSnapshot
	err := busy(w.dlg, t("Backup wird geöffnet…", "Opening the backup…"), func(ctx context.Context) error {
		var err error
		if s, err = recovery.Open(ctx, src); err != nil {
			return err
		}
		sns, err = s.ImageSnapshots(ctx)
		return err
	})
	if err != nil {
		if errors.Is(err, recovery.ErrPassword) {
			err = errors.New(t("Das Passwort bzw. der Wiederherstellungsschlüssel stimmt nicht.", "The password or recovery key is wrong."))
		}
		errorBox(w.dlg, err)
		return err
	}
	if len(sns) == 0 {
		_ = s.Close()
		err := errors.New(t("In diesem Backup-Speicher gibt es keine Disk-Images.", "This backup storage holds no disk images."))
		errorBox(w.dlg, err)
		return err
	}
	if w.session != nil {
		_ = w.session.Close()
	}
	w.session, w.snaps = s, sns
	w.location.SetText(src.Location)
	var labels []string
	for _, sn := range sns {
		l := fmt.Sprintf("%s   %s", sn.Time().Local().Format("2006-01-02 15:04"), sn.Snapshot.Hostname)
		if sn.Guest != "" {
			l += "   VM " + sn.Guest
		}
		var size uint64
		for _, im := range sn.Snapshot.Images {
			size += im.Size
		}
		l += tf("   %d Disk(s), %s", "   %d disk(s), %s", len(sn.Snapshot.Images), humanBytes(size))
		labels = append(labels, l)
	}
	_ = w.snapList.SetModel(labels)
	_ = w.snapList.SetCurrentIndex(0)
	if w.page == 0 {
		w.show(1)
	}
	return nil
}

// --- Snapshot ---------------------------------------------------------------

func (w *wizard) snap() *repo.Snapshot {
	i := w.snapList.CurrentIndex()
	if i < 0 || i >= len(w.snaps) {
		return nil
	}
	return w.snaps[i].Snapshot
}

func (w *wizard) img() *repo.DiskImage {
	sn := w.snap()
	if sn == nil {
		return nil
	}
	i := w.imageList.CurrentIndex()
	if i < 0 || i >= len(sn.Images) {
		return nil
	}
	return &sn.Images[i]
}

func (w *wizard) snapSelected() {
	sn := w.snap()
	if sn == nil {
		return
	}
	var names []string
	for _, im := range sn.Images {
		names = append(names, tf("Disk %d  %s  %s  %s", "Disk %d  %s  %s  %s", im.Number, strings.TrimSpace(im.Model), humanBytes(im.Size), strings.ToUpper(im.Style)))
	}
	_ = w.imageList.SetModel(names)
	_ = w.imageList.SetCurrentIndex(0)
	w.imageSelected()
}

func (w *wizard) imageSelected() {
	im := w.img()
	if im == nil {
		return
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, %s, %s\r\n\r\n", t("Disk", "Disk"), strings.TrimSpace(im.Model), humanBytes(im.Size), strings.ToUpper(im.Style))
	for _, p := range im.Partitions {
		state := t("gesichert", "backed up")
		if !p.Included {
			state = t("nur Layout", "layout only")
		}
		label := strings.Join(p.MountPoints, " ")
		if p.Label != "" {
			label += " \"" + p.Label + "\""
		}
		fmt.Fprintf(&b, "  %d   %-10s  %-6s  %-24s  %s\r\n", p.Number, humanBytes(p.Length), p.FileSystem, strings.TrimSpace(label), state)
	}
	w.snapInfo.SetText(b.String())
	w.limits = nil
	w.layout = nil
}

func (w *wizard) checkSnapshot() error {
	if w.img() == nil {
		return errors.New(t("Bitte ein Backup und eine Disk wählen.", "Choose a backup and a disk."))
	}
	w.loadDisks()
	return nil
}

// --- Target -----------------------------------------------------------------

func (w *wizard) loadDisks() {
	disks, err := imaging.ListDisks()
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	paths := []string{w.location.Text()}
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, exe)
	}
	for _, p := range recovery.FindAccessPackages(recovery.DriveRoots()) {
		paths = append(paths, p.Path)
	}
	w.disks = disks
	w.protected = recovery.ProtectedDisks(disks, paths)
	var labels []string
	for _, d := range disks {
		l := fmt.Sprintf("Disk %d   %s   %s   %s", d.Number, strings.TrimSpace(d.Model), humanBytes(d.Size), strings.ToUpper(d.Style))
		if d.Bus != "" {
			l += "   " + strings.ToUpper(d.Bus)
		}
		var parts []string
		for _, p := range d.Partitions {
			parts = append(parts, strings.TrimSpace(p.Describe()))
		}
		if len(parts) > 0 {
			l += "   [" + strings.Join(parts, "; ") + "]"
		}
		if why, ok := w.protected[d.Number]; ok {
			l += tf("   – gesperrt (%s)", "   – locked (%s)", why)
		}
		if d.System {
			l += t("   – laufendes Windows", "   – running Windows")
		}
		labels = append(labels, l)
	}
	_ = w.diskList.SetModel(labels)
}

func (w *wizard) target() *imaging.Disk {
	i := w.diskList.CurrentIndex()
	if i < 0 || i >= len(w.disks) {
		return nil
	}
	return &w.disks[i]
}

func (w *wizard) targetSelected() { w.modeChanged() }

func (w *wizard) modeChanged() {
	d := w.target()
	mode := w.modeBox.CurrentIndex()
	w.partBox.SetEnabled(mode == modeReplace && d != nil)
	var names []string
	if d != nil && mode == modeReplace {
		for _, p := range d.Partitions {
			names = append(names, fmt.Sprintf("%d  %s  %s", p.Number, humanBytes(p.Length), strings.TrimSpace(p.Describe())))
		}
	}
	_ = w.partBox.SetModel(names)
	if len(names) > 0 {
		_ = w.partBox.SetCurrentIndex(0)
	}
	w.layout = nil
}

func (w *wizard) checkTarget() error {
	d := w.target()
	if d == nil {
		return errors.New(t("Bitte eine Zieldisk wählen.", "Choose a target disk."))
	}
	if why, ok := w.protected[d.Number]; ok {
		return errors.New(tf("Disk %d enthält %s und kann nicht das Ziel sein.", "Disk %d holds %s and cannot be the target.", d.Number, why))
	}
	if d.SectorSize != w.img().SectorSize {
		return errors.New(tf("Die Zieldisk hat %d-Byte-Sektoren, das Backup %d.", "The target disk has %d byte sectors, the backup %d.", d.SectorSize, w.img().SectorSize))
	}
	if w.modeBox.CurrentIndex() == modeReplace && w.partBox.CurrentIndex() < 0 {
		return errors.New(t("Bitte die Partition wählen, die überschrieben wird.", "Choose the partition to overwrite."))
	}
	w.prepareLayoutPage()
	return nil
}

// --- Layout -----------------------------------------------------------------

func (w *wizard) single() bool { return w.modeBox.CurrentIndex() != modeWholeDisk }

func (w *wizard) prepareLayoutPage() {
	single := w.single()
	w.styleBox.SetEnabled(!single)
	w.rows.Parent().AsWindowBase().SetVisible(true)
	w.singleInfo.SetVisible(single)
	w.sizeEdit.SetVisible(single)
	if w.limits == nil {
		img := w.img()
		var lim map[int]imaging.PartitionLimits
		_ = busy(w.dlg, t("Partitionen im Backup werden untersucht (Mindestgrößen)…", "Examining the partitions in the backup (minimum sizes)…"), func(ctx context.Context) error {
			lim = imaging.ImageLimits(ctx, w.session.Repo, img)
			return nil
		})
		w.limits = lim
	}
	if w.layout == nil {
		w.makeLayout(imaging.LayoutFit)
	}
}

// makeLayout proposes a layout for the target disk.
func (w *wizard) makeLayout(mode imaging.LayoutMode) {
	img, d := w.img(), w.target()
	if img == nil || d == nil || w.limits == nil {
		return
	}
	w.clearRows()
	if w.single() {
		w.layout = nil
		w.buildSingle()
		w.view.Invalidate()
		return
	}
	opts := imaging.LayoutOptions{Mode: mode}
	switch w.styleBox.CurrentIndex() {
	case 1:
		opts.Style = imaging.StyleGPT
	case 2:
		opts.Style = imaging.StyleMBR
	}
	if opts.Style == img.Style {
		opts.Style = ""
	}
	if opts.Style != "" {
		opts.Mode = imaging.LayoutFit
		for i := range img.Partitions {
			if imaging.HasWindows(context.Background(), w.session.Repo, &img.Partitions[i]) {
				opts.WindowsPartition = img.Partitions[i].Number
			}
		}
	}
	l, err := imaging.SuggestLayout(img, d.Size, w.limits, opts)
	if err != nil {
		w.layout = nil
		w.layoutErr.SetText(err.Error())
		w.view.Invalidate()
		return
	}
	w.layout = l
	w.buildRows()
	w.validate()
}

func (w *wizard) clearRows() {
	for w.rows.Children().Len() > 0 {
		w.rows.Children().At(0).Dispose()
	}
}

// buildRows creates one row per partition: restore yes/no, size, limits.
func (w *wizard) buildRows() {
	img := w.img()
	for i := range w.layout.Partitions {
		idx := i
		lp := w.layout.Partitions[i]
		var desc, info string
		resizable := false
		switch lp.Source {
		case imaging.SourceNewEFI:
			desc = t("Neu: EFI-Systempartition", "New: EFI system partition")
		case imaging.SourceNewMSR:
			desc = t("Neu: MSR", "New: MSR")
		default:
			var src *repo.PartitionImage
			for k := range img.Partitions {
				if img.Partitions[k].Number == lp.Source {
					src = &img.Partitions[k]
				}
			}
			desc = fmt.Sprintf("%d  %s %s %s", src.Number, strings.Join(src.MountPoints, " "), src.Label, src.FileSystem)
			lim := w.limits[lp.Source]
			resizable = lim.Resizable && lp.Restore
			switch {
			case !src.Included:
				info = t("nicht gesichert, wird leer angelegt", "not backed up, created empty")
			case lim.Why == imaging.ReasonBitLocker:
				info = reasonText(lim, "")
			case !lim.Resizable:
				info = t("nur verschiebbar: ", "can only be moved: ") + reasonText(lim, imaging.Partition{GPTType: src.GPTType, MBRType: src.MBRType}.Kind())
			default:
				info = tf("belegt %s, mindestens %s", "used %s, at least %s", humanBytes(lim.UsedBytes), humanBytes(lim.Min(src)))
			}
		}
		var cb *walk.CheckBox
		var ne *walk.NumberEdit
		err := Composite{
			Layout: HBox{MarginsZero: true},
			Children: []Widget{
				CheckBox{AssignTo: &cb, Text: t("wiederherstellen", "restore"), Checked: lp.Restore, Enabled: lp.Source > 0 && w.included(lp.Source),
					OnCheckedChanged: func() {
						w.layout.Partitions[idx].Restore = cb.Checked()
						w.validate()
					}},
				Label{Text: strings.TrimSpace(desc), MinSize: Size{Width: 220}},
				NumberEdit{AssignTo: &ne, Decimals: 2, Suffix: " GiB", MinValue: 0.01, MaxValue: 1 << 20, Value: gib(lp.Length), Enabled: resizable, MinSize: Size{Width: 110},
					OnValueChanged: func() {
						if err := w.layout.Resize(w.layout.Partitions[idx].Source, fromGiB(ne.Value())); err == nil {
							w.validate()
						}
					}},
				Label{Text: info},
				HSpacer{},
			},
		}.Create(NewBuilder(w.rows))
		if err != nil {
			w.layoutErr.SetText(err.Error())
		}
	}
	if isWinBitLockerVSS(img) {
		w.layoutErr.SetText(t("Hinweis: War eine Partition mit BitLocker verschlüsselt und wurde über VSS gesichert, ist sie nach dem Restore unverschlüsselt. BitLocker danach in Windows wieder einschalten.",
			"Note: a partition encrypted with BitLocker and backed up through VSS is unencrypted after the restore. Turn BitLocker on again in Windows afterwards."))
	}
}

// isWinBitLockerVSS: the image cannot tell whether a plain NTFS partition
// was BitLocker-encrypted; the general note is shown for Windows images.
func isWinBitLockerVSS(img *repo.DiskImage) bool {
	for _, p := range img.Partitions {
		if p.Source == "vss" && strings.EqualFold(p.FileSystem, "NTFS") {
			return true
		}
	}
	return false
}

func (w *wizard) included(num int) bool {
	for _, p := range w.img().Partitions {
		if p.Number == num {
			return p.Included
		}
	}
	return false
}

// buildSingle shows the size of the single partition to restore.
func (w *wizard) buildSingle() {
	img, d := w.img(), w.target()
	var names []string
	var nums []int
	for _, p := range img.Partitions {
		if p.Included {
			names = append(names, fmt.Sprintf("%d  %s  %s %s", p.Number, humanBytes(p.Length), strings.Join(p.MountPoints, " "), p.FileSystem))
			nums = append(nums, p.Number)
		}
	}
	k, ok := askChoice(w.dlg, t("Partition", "Partition"), t("Welche Partition aus dem Backup?", "Which partition of the backup?"), names)
	if !ok {
		k = 0
	}
	src := nums[k]
	size := uint64(0)
	for _, p := range img.Partitions {
		if p.Number == src {
			size = p.Length
		}
	}
	if w.modeBox.CurrentIndex() == modeReplace {
		if i := w.partBox.CurrentIndex(); i >= 0 && i < len(d.Partitions) {
			size = d.Partitions[i].Length
		}
	}
	w.singleSource = src
	lim := w.limits[src]
	w.singleInfo.SetText(tf("Partition %d – Größe auf dem Ziel (mindestens %s):", "Partition %d – size on the target (at least %s):", src, humanBytes(lim.Min(&repo.PartitionImage{Length: size}))))
	_ = w.sizeEdit.SetValue(gib(size))
	w.singleSizeChanged()
}

func (w *wizard) singleSizeChanged() {
	if !w.single() {
		return
	}
	w.layoutErr.SetText("")
	if err := w.checkSingle(); err != nil {
		w.layoutErr.SetText(err.Error())
	}
	w.view.Invalidate()
}

func (w *wizard) partitionTarget() *recovery.PartitionTarget {
	pt := &recovery.PartitionTarget{Partition: w.singleSource, Length: fromGiB(w.sizeEdit.Value()) / 512 * 512}
	if w.modeBox.CurrentIndex() == modeReplace {
		d := w.target()
		if i := w.partBox.CurrentIndex(); i >= 0 && i < len(d.Partitions) {
			pt.Replace = d.Partitions[i].Number
		}
	}
	return pt
}

func (w *wizard) checkSingle() error {
	return imaging.CheckPartitionRestore(w.img(), w.target(), imaging.PartitionRestoreOptions{
		Partition: w.singleSource, Replace: w.partitionTarget().Replace, Length: w.partitionTarget().Length,
	})
}

func (w *wizard) validate() {
	w.layoutErr.SetText("")
	if w.layout != nil {
		if err := w.layout.Validate(w.img(), w.limits); err != nil {
			w.layoutErr.SetText(err.Error())
		}
	}
	w.view.Invalidate()
}

func (w *wizard) checkLayout() error {
	if w.single() {
		if err := w.checkSingle(); err != nil {
			return err
		}
	} else {
		if w.layout == nil {
			return errors.New(w.layoutErr.Text())
		}
		if err := w.layout.Validate(w.img(), w.limits); err != nil {
			return err
		}
	}
	same := !w.single() && recovery.SameDiskAsSource(w.img(), *w.target())
	w.sameDiskHt.SetVisible(same)
	if same {
		w.verify.SetChecked(true)
	}
	return nil
}

// --- Options and summary ------------------------------------------------------

func (w *wizard) job() recovery.Job {
	sn := w.snap()
	j := recovery.Job{Snapshot: sn, Image: w.imageList.CurrentIndex(), TargetDisk: w.target().Number,
		NewHardware: w.newHW.Checked(), KeepOffline: w.keepOff.Checked(), Verify: w.verify.Checked()}
	if d := strings.TrimSpace(w.driverDir.Text()); d != "" {
		j.DriverDirs = []string{d}
	}
	if w.single() {
		j.Into = w.partitionTarget()
	} else if w.layout != nil && (w.layout.Moved(w.img()) || !allRestored(w.layout)) {
		j.Layout = w.layout
	}
	return j
}

func allRestored(l *imaging.Layout) bool {
	for _, p := range l.Partitions {
		if !p.Restore && p.Source > 0 {
			return false
		}
	}
	return true
}

func (w *wizard) checkOptions() error {
	if d := strings.TrimSpace(w.driverDir.Text()); d != "" {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			return errors.New(tf("Der Treiberordner %s wurde nicht gefunden.", "The driver folder %s was not found.", d))
		}
	}
	j := w.job()
	if err := j.Check(w.disks, w.protected); err != nil {
		return err
	}
	d, img, sn := w.target(), w.img(), w.snap()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, %s %s\r\n", t("Backup:", "Backup:"), sn.Time.Local().Format("2006-01-02 15:04"), sn.Hostname, strings.TrimSpace(img.Model))
	fmt.Fprintf(&b, "%s Disk %d, %s, %s\r\n\r\n", t("Ziel:", "Target:"), d.Number, strings.TrimSpace(d.Model), humanBytes(d.Size))
	switch {
	case j.Into != nil && j.Into.Replace > 0:
		fmt.Fprintf(&b, t("Partition %d des Backups ÜBERSCHREIBT Partition %d der Zieldisk (%s). Die anderen Partitionen bleiben.\r\n",
			"Partition %d of the backup OVERWRITES partition %d of the target disk (%s). The other partitions stay.\r\n"), j.Into.Partition, j.Into.Replace, humanBytes(j.Into.Length))
	case j.Into != nil:
		fmt.Fprintf(&b, t("Partition %d des Backups kommt in freien Platz der Zieldisk (%s). Die anderen Partitionen bleiben.\r\n",
			"Partition %d of the backup goes into free space on the target disk (%s). The other partitions stay.\r\n"), j.Into.Partition, humanBytes(j.Into.Length))
	default:
		b.WriteString(t("ALLE DATEN AUF DER ZIELDISK GEHEN VERLOREN.\r\n\r\n", "ALL DATA ON THE TARGET DISK WILL BE LOST.\r\n\r\n"))
		l := w.layout
		if l != nil {
			fmt.Fprintf(&b, "%s %s\r\n", t("Partitionstabelle:", "Partition table:"), strings.ToUpper(l.Style))
			for _, lp := range l.Partitions {
				what := t("wiederherstellen", "restore")
				if !lp.Restore {
					what = t("leer anlegen", "create empty")
				}
				name := strconv.Itoa(lp.Source)
				switch lp.Source {
				case imaging.SourceNewEFI:
					name = "EFI (" + t("neu", "new") + ")"
				case imaging.SourceNewMSR:
					name = "MSR (" + t("neu", "new") + ")"
				}
				fmt.Fprintf(&b, "  %-10s %10s   %s\r\n", name, humanBytes(lp.Length), what)
			}
		}
	}
	b.WriteString("\r\n")
	if j.NewHardware {
		b.WriteString(t("Für neue Hardware vorbereiten: ja\r\n", "Prepare for new hardware: yes\r\n"))
	}
	if j.Verify {
		b.WriteString(t("Backup vorher prüfen: ja\r\n", "Check the backup first: yes\r\n"))
	}
	w.summary.SetText(b.String())
	w.confirm.SetText("")
	return nil
}

// --- Run ---------------------------------------------------------------------

func (w *wizard) start() error {
	d := w.target()
	if strings.TrimSpace(w.confirm.Text()) != strconv.Itoa(d.Number) {
		return errors.New(tf("Zum Bestätigen bitte %d eintippen.", "Type %d to confirm.", d.Number))
	}
	j := w.job()
	w.show(len(w.pages) - 1)
	w.running = true
	w.back.SetEnabled(false)
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	logFile := openLogFile()
	logger := recovery.NewLogger(logFile, func(s string) {
		w.dlg.Synchronize(func() { w.log.AppendText(time.Now().Format("15:04:05 ") + logText(s) + "\r\n") })
	})
	started := time.Now()
	progress := func(phase string, done, total uint64) {
		w.dlg.Synchronize(func() {
			var name string
			switch phase {
			case "verify":
				name = t("Backup wird geprüft", "Checking the backup")
			case "write":
				name = t("Daten werden geschrieben", "Writing data")
			case "hardware":
				name = t("Anpassung an die Hardware", "Adapting to the hardware")
			}
			if total > 0 {
				w.progress.SetValue(int(done * 1000 / total))
				rate := float64(done) / max(time.Since(started).Seconds(), 1)
				rest := time.Duration(float64(total-done)/max(rate, 1)) * time.Second
				name += tf(" – %s von %s, %s/s, noch etwa %s", " – %s of %s, %s/s, about %s left", humanBytes(done), humanBytes(total), humanBytes(uint64(rate)), rest.Round(time.Minute))
			}
			w.phase.SetText(name)
		})
	}
	go func() {
		res, err := recovery.Run(ctx, w.session, j, progress, logger)
		if logFile != nil {
			_ = logFile.Close()
		}
		w.dlg.Synchronize(func() {
			w.running = false
			w.progress.SetValue(1000)
			if err != nil {
				w.phase.SetText(t("Fehlgeschlagen: ", "Failed: ") + err.Error())
				errorBox(w.dlg, err)
				return
			}
			msg := t("Fertig. Die Wiederherstellung ist abgeschlossen.", "Done. The restore has finished.")
			if len(res.Warnings) > 0 {
				msg += "\r\n\r\n" + t("Hinweise:", "Notes:") + "\r\n- " + strings.Join(res.Warnings, "\r\n- ")
			}
			w.phase.SetText(t("Fertig", "Done"))
			walk.MsgBox(w.dlg, "BackupZit", msg, walk.MsgBoxIconInformation)
		})
	}()
	return nil
}

// openLogFile opens a protocol file next to the program or on the first
// writable drive that has a backupzit folder (the recovery medium).
func openLogFile() *os.File {
	name := "restore-" + time.Now().Format("20060102-150405") + ".log"
	var dirs []string
	for _, r := range recovery.DriveRoots() {
		dirs = append(dirs, filepath.Join(r, "backupzit"))
	}
	dirs = append(dirs, os.TempDir())
	for _, d := range dirs {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			continue
		}
		if f, err := os.OpenFile(filepath.Join(d, name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil { // #nosec G304 -- fixed folder
			return f
		}
	}
	return nil
}
