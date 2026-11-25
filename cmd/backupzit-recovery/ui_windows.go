//go:build windows

package main

import (
	"cmp"
	"context"
	"slices"
	"strings"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// gib converts bytes to GiB for the size fields.
func gib(b uint64) float64 { return float64(b) / (1 << 30) }

// fromGiB converts the GiB of a size field back to bytes.
func fromGiB(g float64) uint64 { return uint64(g * (1 << 30)) }

// busy runs fn in the background while a small window says what is
// happening; it returns fn's error.
func busy(owner walk.Form, text string, fn func(ctx context.Context) error) error {
	var dlg *walk.Dialog
	var result error
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := dcl.Dialog{
		AssignTo:   &dlg,
		Title:      "BackupZit",
		MinSize:    dcl.Size{Width: 460, Height: 140},
		Background: solid(colCard),
		Font:       baseFont,
		Layout:     dcl.VBox{Margins: dcl.Margins{Left: 28, Top: 24, Right: 28, Bottom: 24}, Spacing: 14},
		Children: []dcl.Widget{
			dcl.Label{Text: text, Font: dcl.Font{Family: uiFamily, PointSize: 11, Bold: true}, TextColor: colText},
			dcl.ProgressBar{MarqueeMode: true, MinSize: dcl.Size{Height: 8}, MaxSize: dcl.Size{Height: 8}},
		},
	}.Create(owner)
	if err != nil {
		return fn(ctx)
	}
	done := false
	dlg.Closing().Attach(func(canceled *bool, _ walk.CloseReason) {
		if !done {
			*canceled = true
		}
	})
	dlg.Starting().Attach(func() {
		go func() {
			e := fn(ctx)
			dlg.Synchronize(func() {
				result, done = e, true
				dlg.Accept()
			})
		}()
	})
	dlg.Run()
	return result
}

// askText asks for one line of text. Values are read before the dialog
// closes: walk destroys its controls on close, and reading them afterwards
// yields "" (or index 0).
func askText(owner walk.Form, title, prompt string, password bool) (string, bool) {
	var dlg *walk.Dialog
	var edit *walk.LineEdit
	var ok, cancel *walk.PushButton
	var text string
	accept := func() {
		text = edit.Text()
		dlg.Accept()
	}
	abort := func() { dlg.Cancel() }
	if _, err := (dcl.Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       dcl.Size{Width: 520, Height: 260},
		Background:    solid(colBg),
		Font:          baseFont,
		Layout:        dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: dialogFrame(title, "",
			[]dcl.Widget{
				dcl.Label{Text: prompt, TextColor: colText},
				dcl.LineEdit{AssignTo: &edit, PasswordMode: password, MinSize: dcl.Size{Height: 26}},
			},
			[]dcl.Widget{
				dcl.HSpacer{},
				FlatButton{Button: &cancel, Text: t("Abbrechen", "Cancel"), OnClicked: abort},
				FlatButton{Button: &ok, Text: "OK", Kind: btnPrimary, OnClicked: accept},
			}),
	}).Run(owner); err != nil {
		return "", false
	}
	if dlg.Result() != walk.DlgCmdOK {
		return "", false
	}
	return text, true
}

// askChoice lets the user pick one of items.
func askChoice(owner walk.Form, title, prompt string, items []string) (int, bool) {
	if len(items) == 0 {
		return 0, false
	}
	var dlg *walk.Dialog
	var lb *walk.ListBox
	var ok, cancel *walk.PushButton
	choice := -1
	accept := func() {
		choice = lb.CurrentIndex()
		dlg.Accept()
	}
	abort := func() { dlg.Cancel() }
	if _, err := (dcl.Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       dcl.Size{Width: 600, Height: 420},
		Background:    solid(colBg),
		Font:          baseFont,
		Layout:        dcl.VBox{MarginsZero: true, SpacingZero: true},
		Children: dialogFrame(title, "",
			[]dcl.Widget{
				dcl.Label{Text: prompt, TextColor: colText},
				dcl.ListBox{AssignTo: &lb, Model: items, CurrentIndex: 0, OnItemActivated: accept},
			},
			[]dcl.Widget{
				dcl.HSpacer{},
				FlatButton{Button: &cancel, Text: t("Abbrechen", "Cancel"), OnClicked: abort},
				FlatButton{Button: &ok, Text: "OK", Kind: btnPrimary, OnClicked: accept},
			}),
	}).Run(owner); err != nil {
		return 0, false
	}
	if dlg.Result() != walk.DlgCmdOK || choice < 0 {
		return 0, false
	}
	return choice, true
}

// browseFolder lets the user pick a folder.
func browseFolder(owner walk.Form, title string) string {
	dlg := new(walk.FileDialog)
	dlg.Title = title
	if ok, err := dlg.ShowBrowseFolder(owner); err != nil || !ok {
		return ""
	}
	return dlg.FilePath
}

var (
	colPartEFI      = walk.RGB(0x3B, 0x82, 0xF6)
	colPartMSR      = walk.RGB(0x94, 0xA3, 0xB8)
	colPartRecovery = walk.RGB(0xD9, 0x77, 0x06)
	colPartData     = walk.RGB(0x05, 0x96, 0x69)
	colPartEmpty    = walk.RGB(0xCB, 0xD5, 0xE1)
	colPartFaded    = walk.RGB(0xE2, 0xE8, 0xF0)
)

// partColor picks a color by the kind of partition.
func partColor(p *repo.PartitionImage) walk.Color {
	switch strings.ToUpper(p.GPTType) {
	case imaging.GPTTypeEFI:
		return colPartEFI
	case imaging.GPTTypeMSR:
		return colPartMSR
	case imaging.GPTTypeRecovery:
		return colPartRecovery
	}
	switch p.MBRType {
	case 0x27:
		return colPartRecovery
	case 0xEF:
		return colPartEFI
	}
	if !p.Included {
		return colPartEmpty
	}
	return colPartData
}

// bar is one partition in the layout picture: the partition of the
// backup it shows, where it lies, its label, and whether it is drawn grey
// (left as it is, or not restored).
type bar struct {
	src    *repo.PartitionImage
	offset uint64
	length uint64
	label  string
	faded  bool
}

// paintLayout draws the backed up disk and the target disk as bars.
func (w *wizard) paintLayout(c *walk.Canvas, _ walk.Rectangle) error {
	img, d := w.img(), w.target()
	if img == nil || d == nil {
		return nil
	}
	b := w.view.ClientBoundsPixels()
	font := uiFont(9, false)
	gap := px(w.view, 12)
	rowH := (b.Height - gap) / 2
	var srcBars []bar
	for i := range img.Partitions {
		p := &img.Partitions[i]
		srcBars = append(srcBars, bar{src: p, offset: p.Offset, length: p.Length, label: partLabel(p, p.Length)})
	}
	drawBars(c, font, walk.Rectangle{X: 0, Y: 0, Width: b.Width, Height: rowH}, img.Size, t("Backup", "Backup"), srcBars)

	var dstBars []bar
	switch {
	case w.single():
		for _, p := range d.Partitions {
			pp := &repo.PartitionImage{Number: p.Number, GPTType: p.GPTType, MBRType: p.MBRType, Included: true}
			dstBars = append(dstBars, bar{src: pp, offset: p.Offset, length: p.Length, label: strings.TrimSpace(p.Describe()), faded: true})
		}
		pt := w.partitionTarget()
		if img != nil && pt.Partition > 0 {
			var src *repo.PartitionImage
			for i := range img.Partitions {
				if img.Partitions[i].Number == pt.Partition {
					src = &img.Partitions[i]
				}
			}
			if src != nil {
				off := uint64(0)
				if pt.Replace > 0 {
					for _, p := range d.Partitions {
						if p.Number == pt.Replace {
							off = p.Offset
						}
					}
					for i := range dstBars {
						if dstBars[i].src.Number == pt.Replace {
							dstBars = append(dstBars[:i], dstBars[i+1:]...)
							break
						}
					}
				}
				if off > 0 {
					dstBars = append(dstBars, bar{src: src, offset: off, length: pt.Length, label: partLabel(src, pt.Length)})
				}
			}
		}
	case w.layout != nil:
		for _, lp := range w.layout.Partitions {
			src := layoutSourceFor(img, lp)
			dstBars = append(dstBars, bar{src: src, offset: lp.Offset, length: lp.Length, label: partLabel(src, lp.Length), faded: !lp.Restore && lp.Source > 0})
		}
	}
	drawBars(c, font, walk.Rectangle{X: 0, Y: rowH + gap, Width: b.Width, Height: rowH}, d.Size, t("Ziel", "Target"), dstBars)
	return nil
}

// layoutSourceFor is the partition of the backup a partition of the new
// layout comes from; new EFI and MSR partitions get a stand-in so they are
// coloured and labelled like the real ones.
func layoutSourceFor(img *repo.DiskImage, lp imaging.LayoutPartition) *repo.PartitionImage {
	switch lp.Source {
	case imaging.SourceNewEFI:
		return &repo.PartitionImage{GPTType: imaging.GPTTypeEFI, Included: true, Name: "EFI"}
	case imaging.SourceNewMSR:
		return &repo.PartitionImage{GPTType: imaging.GPTTypeMSR, Included: true, Name: "MSR"}
	}
	for i := range img.Partitions {
		if img.Partitions[i].Number == lp.Source {
			return &img.Partitions[i]
		}
	}
	return &repo.PartitionImage{}
}

// partLabel is the text in a partition bar: drive letters, else the
// volume label, else the kind of partition, and the size below.
func partLabel(p *repo.PartitionImage, length uint64) string {
	name := strings.Join(p.MountPoints, " ")
	if name == "" {
		name = p.Label
	}
	if name == "" {
		name = imaging.Partition{GPTType: p.GPTType, MBRType: p.MBRType}.Kind()
	}
	return strings.TrimSpace(name) + "\n" + humanBytes(length)
}

// drawBars draws one disk into r: its title on the left, then the
// partitions as coloured bars placed by barSpans.
func drawBars(c *walk.Canvas, font *walk.Font, r walk.Rectangle, diskSize uint64, title string, bars []bar) {
	scale := func(v int) int { return v * c.DPI() / 96 }
	labelW := scale(76)
	_ = c.DrawTextPixels(title, uiFont(10, true), colMuted, walk.Rectangle{X: r.X, Y: r.Y, Width: labelW, Height: r.Height}, walk.TextLeft|walk.TextVCenter|walk.TextSingleLine)
	area := walk.Rectangle{X: r.X + labelW, Y: r.Y, Width: r.Width - labelW - scale(4), Height: r.Height}
	fill(c, colBg, area, scale(12))
	if diskSize == 0 {
		return
	}
	bars = slices.Clone(bars)
	slices.SortStableFunc(bars, func(a, b bar) int { return cmp.Compare(a.offset, b.offset) })
	parts := make([]span, len(bars))
	for i, br := range bars {
		parts[i] = span{br.offset, br.length}
	}
	sp := barSpans(parts, diskSize, area.Width, 64)
	for i, br := range bars {
		x, wd := area.X+sp[i].x, sp[i].w
		col, textCol := partColor(br.src), colWhite
		if br.faded {
			col, textCol = colPartFaded, colMuted
		}
		rect := walk.Rectangle{X: x + scale(1), Y: area.Y + scale(3), Width: wd - scale(2), Height: area.Height - scale(6)}
		fill(c, col, rect, scale(10))
		if wd > scale(40) {
			_ = c.DrawTextPixels(br.label, font, textCol, walk.Rectangle{X: rect.X + scale(8), Y: rect.Y + scale(6), Width: rect.Width - scale(12), Height: rect.Height - scale(10)}, walk.TextLeft|walk.TextTop|walk.TextWordbreak|walk.TextEndEllipsis)
		}
	}
}
