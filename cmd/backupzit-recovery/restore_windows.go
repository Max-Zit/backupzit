//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"

	"github.com/max-zit/backupzit/internal/backend"
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
	// frames are what show and hide a page: the page itself, or on a small
	// screen the view it scrolls in.
	frames []walk.Widget
	page   int
	back   *flatButton
	next   *flatButton
	title  *walk.Label
	sub    *walk.Label
	rail   *stepRail

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
	// protectPaths are the paths whose disks are never a target (the
	// repository, the recovery medium, access packages); the job checks
	// them again on the disks as they are when it starts.
	protectPaths []string

	// Layout.
	styleBox     *walk.ComboBox
	view         *walk.CustomWidget
	rows         *walk.Composite
	layoutErr    *walk.TextLabel
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
	sameDiskHt *walk.TextLabel
	// sourceHereHt warns when the backed up disk is in this computer next
	// to the target; sourceDisk is its number, -1 when it is not.
	sourceHereHt *walk.TextLabel
	sourceDisk   int

	// Summary and progress.
	summary *walk.TextEdit
	confirm *walk.LineEdit
	// eraseNote names the disk the restore erases or writes to, in red, next to
	// the confirmation.
	eraseNote *walk.Label
	progress  *walk.ProgressBar
	phase     *walk.Label
	percent   *walk.Label
	log       *walk.TextEdit
	running   bool
	cancel    context.CancelFunc
}

// sourceItem is an entry of the source list: a repository found on a
// drive, or an access package.
type sourceItem struct {
	label   string
	repo    string
	pkgPath string
}

// wizardOpened lets tests reach the wizard once its window exists.
var wizardOpened func(*wizard)

func runRestoreWizard(owner walk.Form) {
	w := &wizard{sourceDisk: -1}
	// On a small screen each page scrolls in its own view (see compact).
	views := make([]*walk.ScrollView, len(pageTitles))
	nPage := 0
	page := func(assign **walk.Composite, children ...dcl.Widget) dcl.Widget {
		c := dcl.Composite{AssignTo: assign, Background: solid(colCard), Layout: dcl.VBox{Margins: dcl.Margins{Left: 24, Top: 20, Right: 24, Bottom: 20}, Spacing: 10}, Children: children}
		if !compact {
			c.Visible = false
			return c
		}
		sv := &views[nPage]
		nPage++
		return scrolling(c, colCard, sv)
	}
	margin := 20
	if compact {
		margin = 10
	}
	var steps []string
	for _, p := range pageTitles {
		name := stepName(t(p[0], p[1]))
		steps = append(steps, name)
	}
	var pSource, pSnap, pTarget, pLayout, pOptions, pSummary, pProgress *walk.Composite
	// Esc works like Close, which keeps a running restore from being closed.
	var closeBtn *walk.PushButton
	err := dcl.Dialog{
		AssignTo:     &w.dlg,
		CancelButton: &closeBtn,
		Title:        t("Image wiederherstellen", "Restore an image"),
		MinSize:      fitScreen(dcl.Size{Width: 1080, Height: 760}, minWindow),
		Background:   solid(colBg),
		Font:         baseFont,
		Layout:       dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: []dcl.Widget{
			header("", "", &w.title, &w.sub),
			dcl.Composite{StretchFactor: stretchFill, Layout: dcl.HBox{MarginsZero: true, SpacingZero: true}, Children: []dcl.Widget{
				unlessCompact(StepRail{AssignTo: &w.rail, Steps: steps}),
				unlessCompact(vline()),
				pagesArea(margin,
					page(&pSource,
						dcl.Label{Text: t("Gefundene Backups und Zugangspakete auf den Laufwerken:", "Backups and access packages found on the drives:")},
						dcl.ListBox{AssignTo: &w.sourceList, MinSize: dcl.Size{Height: 120}, MaxSize: dcl.Size{Height: 160}, OnCurrentIndexChanged: w.sourceSelected},
						row(FlatButton{Text: t("Laufwerke erneut durchsuchen", "Search the drives again"), OnClicked: w.findSources}),
						dcl.GroupBox{
							Title:  t("Speicherort", "Location"),
							Layout: dcl.Grid{Columns: 2},
							Children: []dcl.Widget{
								dcl.Label{Text: t("Ort:", "Location:")},
								dcl.LineEdit{AssignTo: &w.location, CueBanner: `E:\backups\pc1  ·  smb://user@nas/backups/pc1  ·  sftp://...  ·  s3://...`},
								dcl.Label{Text: t("Repo-Passwort oder Wiederherstellungsschlüssel:", "Repository password or recovery key:")},
								dcl.LineEdit{AssignTo: &w.password, PasswordMode: true},
								dcl.Label{Text: t("Speicher-Benutzer / S3-Zugriffsschlüssel:", "Storage user / S3 access key:")},
								dcl.LineEdit{AssignTo: &w.storUser},
								dcl.Label{Text: t("Speicher-Passwort / S3-Geheimschlüssel:", "Storage password / S3 secret key:")},
								dcl.LineEdit{AssignTo: &w.storPass, PasswordMode: true},
							},
						},
						muted(t("Für SMB den Benutzer in den Ort schreiben (smb://benutzer@server/freigabe) oder oben eintragen. Netzwerk einrichten: im Hauptfenster.",
							"For SMB put the user into the location (smb://user@server/share) or enter it above. Network settings: main window.")),
					),
					page(&pSnap,
						dcl.Label{Text: t("Backups mit Disk-Images, das neueste zuerst:", "Backups with disk images, newest first:")},
						dcl.ListBox{AssignTo: &w.snapList, MinSize: dcl.Size{Height: 160}, MaxSize: dcl.Size{Height: 220}, OnCurrentIndexChanged: w.snapSelected},
						dcl.Composite{Layout: dcl.HBox{MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{Text: t("Gesicherte Disk:", "Backed up disk:")},
							dcl.ComboBox{AssignTo: &w.imageList, OnCurrentIndexChanged: w.imageSelected},
							dcl.HSpacer{},
						}},
						dcl.TextEdit{AssignTo: &w.snapInfo, ReadOnly: true, VScroll: true, Font: dcl.Font{Family: "Consolas", PointSize: 9}},
					),
					page(&pTarget,
						dcl.Label{Text: t("Zieldatenträger (alles auf der gewählten Disk bzw. Partition wird überschrieben):", "Target disk (everything on the chosen disk or partition is overwritten):")},
						dcl.ListBox{AssignTo: &w.diskList, MinSize: dcl.Size{Height: 160}, MaxSize: dcl.Size{Height: 260}, OnCurrentIndexChanged: w.targetSelected},
						row(FlatButton{Text: t("Datenträger neu einlesen", "Read the disks again"), OnClicked: w.loadDisks}),
						dcl.Composite{Layout: dcl.Grid{Columns: 2, MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{Text: t("Wiederherstellen:", "Restore:")},
							dcl.ComboBox{AssignTo: &w.modeBox, Model: []string{
								t("Ganze Disk (Zieldisk wird neu angelegt)", "Whole disk (the target disk is recreated)"),
								t("Eine Partition über eine vorhandene Partition", "One partition over an existing partition"),
								t("Eine Partition in freien Platz", "One partition into free space"),
							}, CurrentIndex: 0, OnCurrentIndexChanged: w.modeChanged},
							dcl.Label{Text: t("Partition des Ziels:", "Partition of the target:")},
							dcl.ComboBox{AssignTo: &w.partBox, Enabled: false},
						}},
					),
					page(&pLayout,
						dcl.Composite{Layout: dcl.HBox{MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{Text: t("Partitionstabelle:", "Partition table:")},
							dcl.ComboBox{AssignTo: &w.styleBox, Model: []string{t("wie im Backup", "as in the backup"), "GPT (UEFI)", "MBR (BIOS)"}, CurrentIndex: 0, OnCurrentIndexChanged: func() { w.makeLayout(imaging.LayoutFit) }},
							FlatButton{Text: t("Wie im Backup", "As in the backup"), OnClicked: func() { w.makeLayout(imaging.LayoutAsSource) }},
							FlatButton{Text: t("An Zieldisk anpassen", "Fit to the target disk"), OnClicked: func() { w.makeLayout(imaging.LayoutFit) }},
							dcl.HSpacer{},
						}},
						dcl.CustomWidget{AssignTo: &w.view, MinSize: dcl.Size{Height: 130}, Paint: w.paintLayout, ClearsBackground: true, InvalidatesOnResize: true},
						// Both scroll bars stay allowed (see scrolling): a view that
						// scrolls only up and down is no wider than its rows at their
						// narrowest, so the hint column wrapped at its minimum and the
						// vertical scroll bar then covered its right edge (#61, at
						// 1280x800). Now the rows take the page width.
						dcl.ScrollView{
							Background: solid(colCard),
							Layout:     dcl.VBox{MarginsZero: true},
							MinSize:    dcl.Size{Height: 200},
							Children: []dcl.Widget{
								dcl.Composite{AssignTo: &w.rows, Layout: dcl.VBox{MarginsZero: true, Spacing: 2}},
								dcl.VSpacer{},
							},
						},
						dcl.Composite{Layout: dcl.HBox{MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{AssignTo: &w.singleInfo, Visible: false},
							dcl.NumberEdit{AssignTo: &w.sizeEdit, Visible: false, Decimals: 2, Suffix: " GiB", MinValue: 0.01, MaxValue: 1 << 20, OnValueChanged: w.singleSizeChanged},
							dcl.HSpacer{},
						}},
						dcl.TextLabel{AssignTo: &w.layoutErr, TextColor: colDanger, MinSize: dcl.Size{Width: wrapWidth}},
					),
					page(&pOptions,
						dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &w.newHW, Text: t("Für neue Hardware vorbereiten (anderer Rechner oder Controller: Boot-Treiber, Treiber, Bootdateien)", "Prepare for new hardware (another computer or controller: boot drivers, drivers, boot files)")},
						dcl.Composite{Layout: dcl.HBox{MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{Text: t("Zusätzliche Treiber (Ordner):", "Additional drivers (folder):")},
							dcl.LineEdit{AssignTo: &w.driverDir},
							FlatButton{Text: "…", OnClicked: func() {
								if d := browseFolder(w.dlg, t("Treiberordner", "Driver folder")); d != "" {
									_ = w.driverDir.SetText(d)
								}
							}},
						}},
						dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &w.verify, Text: t("Backup vorher vollständig lesen und prüfen (dauert etwa so lang wie die Wiederherstellung)", "Read and check the whole backup first (takes about as long as the restore)")},
						dcl.TextLabel{AssignTo: &w.sameDiskHt, TextColor: colWarn, Visible: false, MinSize: dcl.Size{Width: wrapWidth},
							Text: t("Die Zieldisk sieht aus wie die gesicherte Disk. Prüfen Sie das Backup vorher: ist es beschädigt, wäre sonst beides verloren.",
								"The target disk looks like the backed up disk. Check the backup first: if it is damaged, both would be lost.")},
						// The backed up disk is in this computer next to the target
						// (its text names the disk; see sourceDiskText).
						dcl.TextLabel{AssignTo: &w.sourceHereHt, TextColor: colWarn, Visible: false, MinSize: dcl.Size{Width: wrapWidth}},
						dcl.CheckBox{Alignment: dcl.AlignHNearVCenter, AssignTo: &w.keepOff, Text: t("Zieldisk danach offline lassen (wenn die alte Disk noch im Rechner steckt)", "Leave the target disk offline (when the old disk is still in the computer)")},
						dcl.VSpacer{},
					),
					page(&pSummary,
						dcl.TextEdit{AssignTo: &w.summary, ReadOnly: true, VScroll: true, MinSize: dcl.Size{Height: 160}, Font: dcl.Font{Family: "Consolas", PointSize: 9}},
						dcl.Label{AssignTo: &w.eraseNote, Font: dcl.Font{Family: uiFamily, PointSize: 12, Bold: true}, TextColor: colDanger},
						dcl.Label{Text: t("Zum Bestätigen die Nummer der Zieldisk eintippen:", "Type the number of the target disk to confirm:"), Font: dcl.Font{Family: uiFamily, PointSize: 10, Bold: true}, TextColor: colDanger},
						dcl.LineEdit{AssignTo: &w.confirm, MaxSize: dcl.Size{Width: 80}},
					),
					page(&pProgress,
						// The percentage keeps room for "100 %" and the phase line
						// gives way with "…" when both do not fit: the percentage was
						// cut to "100" (#61).
						dcl.Composite{Layout: dcl.HBox{MarginsZero: true}, Children: []dcl.Widget{
							dcl.Label{AssignTo: &w.phase, Font: dcl.Font{Family: uiFamily, PointSize: 13, Bold: true}, TextColor: colText, EllipsisMode: dcl.EllipsisEnd},
							dcl.HSpacer{},
							dcl.Label{AssignTo: &w.percent, Font: dcl.Font{Family: uiFamily, PointSize: 20, Bold: true}, TextColor: colAccent, TextAlignment: dcl.AlignFar, MinSize: dcl.Size{Width: 110}},
						}},
						dcl.ProgressBar{AssignTo: &w.progress, MinValue: 0, MaxValue: 1000, MinSize: dcl.Size{Height: 22}},
						section(t("Protokoll", "Log")),
						dcl.TextEdit{AssignTo: &w.log, ReadOnly: true, VScroll: true, Font: dcl.Font{Family: "Consolas", PointSize: 9}},
					),
				),
			}},
			footer(
				dcl.HSpacer{},
				FlatButton{AssignTo: &w.back, Text: t("Zurück", "Back"), OnClicked: func() { w.show(w.page - 1) }},
				FlatButton{AssignTo: &w.next, Kind: btnPrimary, Text: t("Weiter", "Next"), OnClicked: w.forward},
				FlatButton{Button: &closeBtn, Text: t("Schließen", "Close"), OnClicked: w.close},
			),
		},
	}.Create(owner)
	if err != nil {
		errorBox(owner, err)
		return
	}
	w.pages = []*walk.Composite{pSource, pSnap, pTarget, pLayout, pOptions, pSummary, pProgress}
	w.frames = make([]walk.Widget, len(w.pages))
	for i, p := range w.pages {
		w.frames[i] = p
		if compact {
			w.frames[i] = views[i]
		}
	}
	w.dlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if w.running {
			*canceled = true
		}
	})
	w.show(0)
	w.dlg.Starting().Attach(w.findSources)
	if wizardOpened != nil {
		wizardOpened(w)
	}
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
	for k, f := range w.frames {
		f.SetVisible(k == i)
	}
	w.page = i
	name := stepName(t(pageTitles[i][0], pageTitles[i][1]))
	_ = w.title.SetText(name)
	_ = w.sub.SetText(fmt.Sprintf("%s  ·  %d / %d", t("Image wiederherstellen", "Restore an image"), i+1, len(w.pages)))
	w.rail.SetCurrent(i)
	w.back.SetEnabled(i > 0 && i < len(w.pages)-1)
	w.next.SetVisible(i < len(w.pages)-1)
	w.next.SetText(t("Weiter", "Next"))
	w.next.SetKind(btnPrimary)
	if i == len(w.pages)-2 {
		w.next.SetText(t("Wiederherstellen", "Restore"))
		w.next.SetKind(btnDanger)
	}
	// On the summary the typed confirmation comes first, so the focus does
	// not wait on Restore.
	if i == len(w.pages)-2 && w.confirm.Visible() && w.confirm.Enabled() {
		_ = w.confirm.SetFocus()
		return
	}
	// Keys must not go to a control of the page just hidden (after a click
	// or a dialog the focus stays there, and Tab does nothing): the focus then
	// moves to the first control of the new page, else to Next.
	if f := win.GetFocus(); f == 0 || !win.IsWindowVisible(f) {
		if c := firstFocusable(w.pages[i]); c != nil {
			_ = c.SetFocus()
		} else if w.next.pb.Visible() {
			_ = w.next.pb.SetFocus()
		}
	}
}

// firstFocusable is the first visible, enabled control of c that takes the
// keyboard, depth first; nil if there is none.
func firstFocusable(c walk.Container) walk.Widget {
	ch := c.Children()
	for k := 0; k < ch.Len(); k++ {
		wd := ch.At(k)
		if !wd.Visible() || !wd.Enabled() {
			continue
		}
		switch x := wd.(type) {
		case *walk.ListBox, *walk.LineEdit, *walk.ComboBox, *walk.CheckBox, *walk.NumberEdit, *walk.PushButton:
			return x
		case walk.Container:
			if f := firstFocusable(x); f != nil {
				return f
			}
		}
	}
	return nil
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
		_ = w.location.SetText(s.repo)
		return
	}
	pass, ok := askText(w.dlg, t("Zugangspaket", "Access package"), t("Passphrase des Zugangspakets:", "Passphrase of the access package:"), true)
	if !ok {
		return
	}
	b, err := recovery.ReadAccessPackageFile(s.pkgPath) // size-checked before it is read
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
	_ = w.openWith(src, cleanup) // it shows its own error
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
	// An SFTP server whose host key is not known yet: show the key and ask
	// whether to trust it for this restore, as ssh does on first contact.
	// An access package brings the key along and never gets here.
	var unknown *backend.ErrHostKeyUnknown
	if errors.As(err, &unknown) && src.Options.SFTPHostKey == "" {
		host := src.Location
		if u, uerr := url.Parse(src.Location); uerr == nil {
			host = u.Host
		}
		q := tf("Der SFTP-Server %s hat den Host-Schlüssel %s (%s) gezeigt.\r\n\r\nVergleichen Sie ihn mit dem Schlüssel des Servers, z. B. auf dem Server: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\r\n\r\nDiesem Schlüssel für die Wiederherstellung vertrauen?",
			"The SFTP server %s presented the host key %s (%s).\r\n\r\nCompare it with the server's own key, e.g. on the server: ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub\r\n\r\nTrust this key for the restore?",
			host, unknown.Fingerprint, unknown.KeyType)
		if walk.MsgBox(w.dlg, "BackupZit", q, walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != win.IDYES {
			return err
		}
		src.Options.SFTPHostKey = unknown.Fingerprint
		return w.openWith(src, func() {})
	}
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
	_ = w.location.SetText(src.Location)
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
	_ = w.snapInfo.SetText(b.String())
	w.limits = nil
	w.layout = nil
	w.singleSource = 0 // another backup: choose its partition again
}

func (w *wizard) checkSnapshot() error {
	if w.img() == nil {
		return errors.New(t("Bitte ein Backup und eine Disk wählen.", "Choose a backup and a disk."))
	}
	w.loadDisks()
	return nil
}

// --- Target -----------------------------------------------------------------

// protectedPaths are the paths whose disks must not be written: the
// repository (a usb:// location is resolved to its disk), the medium
// Windows PE started from, the program (when started from a stick in a
// normal Windows) and every access package found.
func (w *wizard) protectedPaths() []string {
	paths := append([]string{w.location.Text()}, recovery.BootMediumPaths()...)
	if exe, err := os.Executable(); err == nil {
		paths = append(paths, exe)
	}
	for _, p := range recovery.FindAccessPackages(recovery.DriveRoots()) {
		paths = append(paths, p.Path)
	}
	return paths
}

func (w *wizard) loadDisks() {
	disks, err := imaging.ListDisks()
	if err != nil {
		errorBox(w.dlg, err)
		return
	}
	w.protectPaths = w.protectedPaths()
	w.disks = disks
	w.protected = recovery.ProtectedDisks(disks, w.protectPaths)
	var labels []string
	for _, d := range disks {
		l := fmt.Sprintf("Disk %d   %s   %s   %s", d.Number, strings.TrimSpace(d.Model), humanBytes(d.Size), diskStyleText(d.Style))
		if d.Bus != "" {
			l += "   " + strings.ToUpper(d.Bus)
		}
		// Why a disk cannot be chosen comes before its partitions, so a narrow list
		// still shows it.
		if why, ok := w.protected[d.Number]; ok {
			l += tf("   – gesperrt (%s)", "   – locked (%s)", why)
		}
		if d.System {
			l += t("   – laufendes Windows", "   – running Windows")
		}
		var parts []string
		for _, p := range d.Partitions {
			parts = append(parts, strings.TrimSpace(p.Describe()))
		}
		if len(parts) > 0 {
			l += "   [" + strings.Join(parts, "; ") + "]"
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
		// 1:1 when the backup fits the disk as it was, the usual restore
		// onto the original disk (fit would grow a partition into a gap and
		// rebuild the boot files); fit only when it does not.
		mode := imaging.LayoutFit
		if img, d := w.img(), w.target(); img != nil && d != nil {
			if _, err := imaging.SuggestLayout(img, d.Size, w.limits, imaging.LayoutOptions{Mode: imaging.LayoutAsSource}); err == nil {
				mode = imaging.LayoutAsSource
			}
		}
		w.makeLayout(mode)
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
		_ = w.view.Invalidate()
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
		_ = w.layoutErr.SetText(layoutErrText(err))
		_ = w.view.Invalidate()
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
				info = limitsInfo(lim, src)
			}
		}
		var cb *walk.CheckBox
		var ne *walk.NumberEdit
		err := dcl.Composite{
			Layout: dcl.HBox{MarginsZero: true},
			Children: []dcl.Widget{
				dcl.CheckBox{AssignTo: &cb, Text: t("wiederherstellen", "restore"), Checked: lp.Restore, Enabled: lp.Source > 0 && w.included(lp.Source),
					OnCheckedChanged: func() {
						w.layout.Partitions[idx].Restore = cb.Checked()
						w.validate()
					}},
				dcl.Label{Text: strings.TrimSpace(desc), MinSize: dcl.Size{Width: 160}, MaxSize: dcl.Size{Width: 160}},
				dcl.NumberEdit{AssignTo: &ne, Decimals: 2, Suffix: " GiB", MinValue: 0.01, MaxValue: 1 << 20, Value: gib(lp.Length), Enabled: resizable, MinSize: dcl.Size{Width: 110}, MaxSize: dcl.Size{Width: 120},
					OnValueChanged: func() {
						if err := w.layout.Resize(w.layout.Partitions[idx].Source, fromGiB(ne.Value())); err == nil {
							w.validate()
						}
					}},
				// The reason wraps, so a long one never makes the window wider than
				// the screen.
				dcl.TextLabel{Text: info, StretchFactor: 1, MinSize: dcl.Size{Width: 160}},
			},
		}.Create(dcl.NewBuilder(w.rows))
		if err != nil {
			_ = w.layoutErr.SetText(err.Error())
		}
	}
	if isWinBitLockerVSS(img) {
		_ = w.layoutErr.SetText(t("Hinweis: War eine Partition mit BitLocker verschlüsselt und wurde über VSS gesichert, ist sie nach dem Restore unverschlüsselt. BitLocker danach in Windows wieder einschalten.",
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
	// The choice is kept while the backup's partition exists: the layout
	// is rebuilt on many changes, and asking again each time (or taking
	// the first partition, often the EFI one, when the dialog is closed)
	// would change what is restored behind the user's back.
	src := 0
	for _, n := range nums {
		if n == w.singleSource {
			src = n
		}
	}
	if src == 0 {
		k, ok := askChoice(w.dlg, t("Partition", "Partition"), t("Welche Partition aus dem Backup?", "Which partition of the backup?"), names)
		if !ok || k < 0 || k >= len(nums) {
			w.singleSource = 0
			_ = w.singleInfo.SetText(t("Keine Partition aus dem Backup gewählt.", "No partition of the backup chosen."))
			return
		}
		src = nums[k]
	}
	var srcPart *repo.PartitionImage
	for i := range img.Partitions {
		if img.Partitions[i].Number == src {
			srcPart = &img.Partitions[i]
		}
	}
	size := srcPart.Length
	if w.modeBox.CurrentIndex() == modeReplace {
		if i := w.partBox.CurrentIndex(); i >= 0 && i < len(d.Partitions) {
			size = d.Partitions[i].Length
		}
	}
	w.singleSource = src
	lim := w.limits[src]
	// The minimum is the image partition's (what its file system needs),
	// not the size of the partition it replaces.
	_ = w.singleInfo.SetText(tf("Partition %d – Größe auf dem Ziel (mindestens %s):", "Partition %d – size on the target (at least %s):", src, humanBytes(lim.Min(srcPart))))
	_ = w.sizeEdit.SetValue(gib(size))
	w.singleSizeChanged()
}

func (w *wizard) singleSizeChanged() {
	if !w.single() {
		return
	}
	msg := ""
	if err := w.checkSingle(); err != nil {
		msg = err.Error()
	} else if lim := w.limits[w.singleSource]; lim.Reason != "" {
		// Why the minimum above is that large (#61).
		for _, p := range w.img().Partitions {
			if p.Number == w.singleSource {
				msg = heldText(p.Number, lim, imaging.Partition{GPTType: p.GPTType, MBRType: p.MBRType}.Kind())
			}
		}
	}
	_ = w.layoutErr.SetText(msg)
	_ = w.view.Invalidate()
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
	_ = w.layoutErr.SetText("")
	if w.layout != nil {
		if err := w.layout.Validate(w.img(), w.limits); err != nil {
			_ = w.layoutErr.SetText(layoutErrText(err))
		}
	}
	_ = w.view.Invalidate()
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
			return errors.New(layoutErrText(err))
		}
	}
	same := !w.single() && recovery.SameDiskAsSource(w.img(), *w.target())
	w.sameDiskHt.SetVisible(same)
	if same {
		w.verify.SetChecked(true)
	}
	// Restoring next to the backed up disk: say before the restore that
	// Windows keeps the copy offline while both are in (#61).
	w.sourceDisk = -1
	if n, ok := w.job().SourceDiskPresent(w.disks); ok {
		w.sourceDisk = n
		_ = w.sourceHereHt.SetText(sourceDiskText(n))
	}
	w.sourceHereHt.SetVisible(w.sourceDisk >= 0)
	return nil
}

// --- Options and summary ------------------------------------------------------

func (w *wizard) job() recovery.Job {
	sn := w.snap()
	td := w.target()
	id := td.Identity()
	j := recovery.Job{Snapshot: sn, Image: w.imageList.CurrentIndex(), TargetDisk: td.Number, TargetID: &id, Protect: w.protectPaths,
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
	// Check against the disks as they are now: the list may be minutes
	// old, and a disk plugged in again gets another number.
	now, err := imaging.ListDisks()
	if err != nil {
		return err
	}
	if err := j.Check(now, recovery.ProtectedDisks(now, w.protectPaths)); err != nil {
		return err
	}
	d, img, sn := w.target(), w.img(), w.snap()
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s, %s %s\r\n", t("Backup:", "Backup:"), sn.Time.Local().Format("2006-01-02 15:04"), sn.Hostname, strings.TrimSpace(img.Model))
	fmt.Fprintf(&b, "%s Disk %d, %s, %s\r\n\r\n", t("Ziel:", "Target:"), d.Number, strings.TrimSpace(d.Model), humanBytes(d.Size))
	// The disk that is erased or written to, in red next to the confirmation.
	model, size := strings.TrimSpace(d.Model), humanBytes(d.Size)
	switch {
	case j.Into != nil && j.Into.Replace > 0:
		_ = w.eraseNote.SetText(tf("Wird überschrieben: Partition %d von Disk %d · %s · %s", "Will be overwritten: partition %d of Disk %d · %s · %s", j.Into.Replace, d.Number, model, size))
	case j.Into != nil:
		_ = w.eraseNote.SetText(tf("Wird in freien Platz geschrieben: Disk %d · %s · %s", "Will be written into free space of Disk %d · %s · %s", d.Number, model, size))
	default:
		_ = w.eraseNote.SetText(tf("Wird gelöscht: Disk %d · %s · %s", "Will be erased: Disk %d · %s · %s", d.Number, model, size))
	}
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
	if w.sourceDisk >= 0 && !j.KeepOffline {
		b.WriteString("\r\n" + sourceDiskText(w.sourceDisk) + "\r\n")
	}
	_ = w.summary.SetText(b.String())
	_ = w.confirm.SetText("")
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
	logFile := openLogFile(j.TargetDisk)
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
				w.progress.SetValue(int(done * 1000 / total)) // #nosec G115 -- done <= total, so at most 1000
				_ = w.percent.SetText(percentText(done, total))
				rate := float64(done) / max(time.Since(started).Seconds(), 1)
				rest := time.Duration(float64(total-done)/max(rate, 1)) * time.Second
				name += tf(" – %s von %s, %s/s, noch etwa %s", " – %s of %s, %s/s, about %s left", humanBytes(done), humanBytes(total), humanBytes(uint64(rate)), rest.Round(time.Minute))
			}
			_ = w.phase.SetText(name)
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
				_ = w.percent.SetText("")
				w.phase.SetTextColor(colDanger)
				_ = w.phase.SetText(t("Fehlgeschlagen: ", "Failed: ") + err.Error())
				errorBox(w.dlg, err)
				return
			}
			msg := t("Fertig. Die Wiederherstellung ist abgeschlossen.", "Done. The restore has finished.")
			if len(res.Warnings) > 0 {
				notes := make([]string, len(res.Warnings))
				for i, s := range res.Warnings {
					notes[i] = warningText(s)
				}
				msg += "\r\n\r\n" + t("Hinweise:", "Notes:") + "\r\n- " + strings.Join(notes, "\r\n- ")
			}
			_ = w.percent.SetText(percentText(1, 1))
			w.phase.SetTextColor(colOK)
			_ = w.phase.SetText(t("Fertig", "Done"))
			walk.MsgBox(w.dlg, "BackupZit", msg, walk.MsgBoxIconInformation)
		})
	}()
	return nil
}

// openLogFile opens a protocol file in \backupzit on a writable drive
// that is not on the target disk (the stick or the backup disk; the target
// is overwritten while the file is open), else in the temp folder.
func openLogFile(targetDisk int) *os.File {
	name := "restore-" + time.Now().Format("20060102-150405") + ".log"
	f, err := os.OpenFile(filepath.Join(backupDir(targetDisk), name), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) // #nosec G304 -- fixed folder
	if err != nil {
		return nil
	}
	return f
}

// stepNumber is the "1. " in front of a page title.
var stepNumber = regexp.MustCompile(`^\s*\d+\s*[.)．、]\s*`)

// stepName is a page title without its number, for the step list and the
// header; a translation without the number stays whole.
func stepName(title string) string { return stepNumber.ReplaceAllString(title, "") }
