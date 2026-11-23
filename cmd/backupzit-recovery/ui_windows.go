//go:build windows

package main

import (
	"context"
	"strings"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"

	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/repo"
)

// busy runs fn in the background while a small window says what is
// happening; it returns fn's error.
func busy(owner walk.Form, text string, fn func(ctx context.Context) error) error {
	var dlg *walk.Dialog
	var result error
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := Dialog{
		AssignTo: &dlg,
		Title:    "BackupZit",
		MinSize:  Size{Width: 420, Height: 120},
		Layout:   VBox{},
		Children: []Widget{
			Label{Text: text},
			ProgressBar{MarqueeMode: true},
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
	if _, err := (Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       Size{Width: 420, Height: 140},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: prompt},
			LineEdit{AssignTo: &edit, PasswordMode: password},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &ok, Text: "OK", OnClicked: accept},
				PushButton{AssignTo: &cancel, Text: t("Abbrechen", "Cancel"), OnClicked: func() { dlg.Cancel() }},
			}},
		},
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
	if _, err := (Dialog{
		AssignTo:      &dlg,
		Title:         title,
		DefaultButton: &ok,
		CancelButton:  &cancel,
		MinSize:       Size{Width: 520, Height: 300},
		Layout:        VBox{},
		Children: []Widget{
			Label{Text: prompt},
			ListBox{AssignTo: &lb, Model: items, CurrentIndex: 0, OnItemActivated: accept},
			Composite{Layout: HBox{MarginsZero: true}, Children: []Widget{
				HSpacer{},
				PushButton{AssignTo: &ok, Text: "OK", OnClicked: accept},
				PushButton{AssignTo: &cancel, Text: t("Abbrechen", "Cancel"), OnClicked: func() { dlg.Cancel() }},
			}},
		},
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

// partColor picks a color by the kind of partition.
func partColor(p *repo.PartitionImage) walk.Color {
	switch strings.ToUpper(p.GPTType) {
	case imaging.GPTTypeEFI:
		return walk.RGB(0x5B, 0x84, 0xC4)
	case imaging.GPTTypeMSR:
		return walk.RGB(0x9A, 0x9A, 0x9A)
	case imaging.GPTTypeRecovery:
		return walk.RGB(0xC0, 0x8A, 0x2E)
	}
	switch p.MBRType {
	case 0x27:
		return walk.RGB(0xC0, 0x8A, 0x2E)
	case 0xEF:
		return walk.RGB(0x5B, 0x84, 0xC4)
	}
	if !p.Included {
		return walk.RGB(0xB8, 0xB8, 0xB8)
	}
	return walk.RGB(0x3A, 0x7D, 0x44)
}

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
	font := w.dlg.Font()
	rowH := (b.Height - 8) / 2
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
	drawBars(c, font, walk.Rectangle{X: 0, Y: rowH + 8, Width: b.Width, Height: rowH}, d.Size, t("Ziel", "Target"), dstBars)
	return nil
}

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

func drawBars(c *walk.Canvas, font *walk.Font, r walk.Rectangle, diskSize uint64, title string, bars []bar) {
	labelW := 70
	_ = c.DrawTextPixels(title, font, walk.RGB(0x30, 0x30, 0x30), walk.Rectangle{X: r.X, Y: r.Y, Width: labelW, Height: r.Height}, walk.TextLeft|walk.TextVCenter|walk.TextSingleLine)
	area := walk.Rectangle{X: r.X + labelW, Y: r.Y, Width: r.Width - labelW - 4, Height: r.Height}
	if frame, err := walk.NewSolidColorBrush(walk.RGB(0xEE, 0xEE, 0xEE)); err == nil {
		_ = c.FillRectanglePixels(frame, area)
		frame.Dispose()
	}
	if diskSize == 0 {
		return
	}
	minW := 6
	for _, br := range bars {
		x := area.X + int(float64(area.Width)*float64(br.offset)/float64(diskSize))
		wd := int(float64(area.Width) * float64(br.length) / float64(diskSize))
		if wd < minW {
			wd = minW
		}
		col := partColor(br.src)
		if br.faded {
			col = walk.RGB(0xC8, 0xC8, 0xC8)
		}
		brush, err := walk.NewSolidColorBrush(col)
		if err != nil {
			continue
		}
		rect := walk.Rectangle{X: x, Y: area.Y + 2, Width: wd - 1, Height: area.Height - 4}
		_ = c.FillRectanglePixels(brush, rect)
		brush.Dispose()
		if wd > 40 {
			_ = c.DrawTextPixels(br.label, font, walk.RGB(255, 255, 255), walk.Rectangle{X: rect.X + 4, Y: rect.Y + 4, Width: rect.Width - 8, Height: rect.Height - 8}, walk.TextLeft|walk.TextTop|walk.TextWordbreak|walk.TextEndEllipsis)
		}
	}
}
