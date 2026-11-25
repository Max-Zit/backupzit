//go:build windows

package main

import (
	"fmt"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// The tests here drive the windows with keyboard and mouse messages, the
// way Windows delivers them, so they also run with the screen locked.

// onUI runs fn on the window's thread and waits for it.
func onUI(f walk.Form, fn func()) {
	done := make(chan struct{})
	f.Synchronize(func() { fn(); close(done) })
	<-done
}

// press posts a key down and up to the window and lets the thread handle it.
func press(f walk.Form, h win.HWND, vk uintptr) {
	win.PostMessage(h, win.WM_KEYDOWN, vk, 1)
	win.PostMessage(h, win.WM_KEYUP, vk, 1|3<<30)
	settle(f)
}

func settle(f walk.Form) {
	for range 2 {
		onUI(f, func() {})
		time.Sleep(50 * time.Millisecond)
	}
}

// children lists the descendants of parent with the class, in tab order.
func children(parent win.HWND, class string) []win.HWND {
	var found []win.HWND
	cb := syscall.NewCallback(func(h win.HWND, _ uintptr) uintptr {
		buf := make([]uint16, 64)
		win.GetClassName(h, &buf[0], len(buf))
		if syscall.UTF16ToString(buf) == class {
			found = append(found, h)
		}
		return 1
	})
	_, _, _ = procEnumChildWindows.Call(uintptr(parent), cb, 0)
	return found
}

func windowText(h win.HWND) string {
	buf := make([]uint16, 256)
	n := win.SendMessage(h, win.WM_GETTEXT, uintptr(len(buf)), uintptr(unsafe.Pointer(&buf[0])))
	return syscall.UTF16ToString(buf[:n])
}

func buttonWithText(parent win.HWND, text string) win.HWND {
	for _, h := range children(parent, "Button") {
		if windowText(h) == text {
			return h
		}
	}
	return 0
}

const customWidgetClass = `\o/ Walk_CustomWidget_Class \o/`

func TestMainWindowKeyboardAndHover(t *testing.T) {
	useManifest(t)
	withLang(t, "de")
	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 1)
	go func() {
		errs <- func() error {
			defer win.PostMessage(a.mw.Handle(), win.WM_CLOSE, 0, 0)
			if waitWindow("BackupZit Recovery "+version) == 0 {
				return fmt.Errorf("main window not shown")
			}
			mw := a.mw.Handle()
			tiles := children(mw, customWidgetClass)
			var stops []win.HWND
			for _, h := range tiles {
				if win.GetWindowLong(h, win.GWL_STYLE)&win.WS_TABSTOP != 0 {
					stops = append(stops, h)
				}
			}
			if len(stops) != 4 {
				return fmt.Errorf("%d tiles with a tab stop, want 4", len(stops))
			}

			// Tab walks the tiles, then the five footer buttons, and wraps.
			onUI(a.mw, func() { win.SetFocus(stops[0]) })
			var order []string
			for range 10 {
				var f win.HWND
				onUI(a.mw, func() { f = win.GetFocus() })
				press(a.mw, f, win.VK_TAB)
				onUI(a.mw, func() { f = win.GetFocus() })
				order = append(order, windowText(f))
				if f == stops[0] {
					break
				}
			}
			want := []string{"", "", "", "Netzwerk…", "Eingabeaufforderung", "Notepad", "Neu starten", "Herunterfahren", ""}
			if fmt.Sprint(order) != fmt.Sprint(want) {
				return fmt.Errorf("tab order %q, want %q", order, want)
			}

			// Enter on a tile and Space on a tile open their dialogs.
			for _, c := range []struct {
				tile  win.HWND
				key   uintptr
				title string
			}{{stops[1], win.VK_RETURN, "Boot reparieren"}, {stops[2], win.VK_SPACE, "Treiber"}} {
				onUI(a.mw, func() { win.SetFocus(c.tile) })
				win.PostMessage(c.tile, win.WM_KEYDOWN, c.key, 1)
				h := waitWindow(c.title)
				if h == 0 {
					return fmt.Errorf("%s: dialog did not open", c.title)
				}
				waitGone("BackupZit")
				win.PostMessage(h, win.WM_CLOSE, 0, 0)
				time.Sleep(500 * time.Millisecond)
			}

			// Moving the mouse over a tile and away again switches its
			// accent border on and off.
			tile := stops[3]
			onUI(a.mw, func() { win.SetFocus(buttonWithText(mw, "Notepad")) })
			// The mouse is not really there, so Windows reports it leaving
			// right away; record what the tile was told.
			var seen []bool
			onUI(a.mw, func() {
				inner := hoverFn[tile]
				hoverFn[tile] = func(v bool) { seen = append(seen, v); inner(v) }
			})
			win.PostMessage(tile, win.WM_MOUSEMOVE, 0, 20<<16|20)
			settle(a.mw)
			win.PostMessage(tile, win.WM_MOUSELEAVE, 0, 0)
			settle(a.mw)
			var got string
			onUI(a.mw, func() { got = fmt.Sprint(seen) })
			if got != "[true false]" {
				return fmt.Errorf("hover changes %s, want [true false]", got)
			}
			return nil
		}()
	}()
	a.mw.Run()
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}

func TestFlatButtonPaintsStates(t *testing.T) {
	useManifest(t)
	var dlg *walk.Dialog
	var prim, sec *flatButton
	if err := (dcl.Dialog{
		AssignTo:   &dlg,
		Title:      "flat test",
		Background: solid(colCard),
		Layout:     dcl.VBox{},
		Children: []dcl.Widget{
			FlatButton{AssignTo: &prim, Text: "Primär", Kind: btnPrimary},
			FlatButton{AssignTo: &sec, Text: "Neben"},
		},
	}).Create(nil); err != nil {
		t.Fatal(err)
	}
	defer dlg.Dispose()
	dlg.Show()
	// face paints the button into memory and returns a color of its face.
	face := func(f *flatButton) walk.Color {
		screen := win.GetDC(0)
		defer win.ReleaseDC(0, screen)
		mem := win.CreateCompatibleDC(screen)
		defer win.DeleteDC(mem)
		bmp := win.CreateCompatibleBitmap(screen, 120, 38)
		defer win.DeleteObject(win.HGDIOBJ(bmp))
		old := win.SelectObject(mem, win.HGDIOBJ(bmp))
		defer win.SelectObject(mem, old)
		f.paint(&gdi{hdc: mem, bounds: walk.Rectangle{Width: 120, Height: 38}, dpi: 96})
		return walk.Color(win.GetPixel(mem, 12, 8))
	}
	set := func(f *flatButton, hover bool) { f.hover = hover }
	for _, c := range []struct {
		name  string
		f     *flatButton
		apply func()
		want  walk.Color
	}{
		{"primary", prim, func() {}, colAccent},
		{"primary hover", prim, func() { set(prim, true) }, colAccentHi},
		{"danger", prim, func() { set(prim, false); prim.SetKind(btnDanger) }, colDanger},
		{"danger hover", prim, func() { set(prim, true) }, colDangerHi},
		{"disabled", prim, func() { set(prim, false); prim.SetEnabled(false) }, colDisabled},
		{"secondary", sec, func() {}, colCard},
		{"secondary hover", sec, func() { set(sec, true) }, colHover},
	} {
		c.apply()
		if got := face(c.f); got != c.want {
			t.Errorf("%s: face %06x, want %06x", c.name, uint32(got), uint32(c.want))
		}
	}
}

// typeAndPress waits for the dialog, lets prepare fill it in, then sends
// the keys to the control of the class, as a user at the keyboard would.
func typeAndPress(title, class string, prepare func(h win.HWND), keys ...uintptr) {
	go func() {
		dlg := waitWindow(title)
		if dlg == 0 {
			return
		}
		h := childOfClass(dlg, class)
		if prepare != nil {
			prepare(h)
		}
		for _, k := range keys {
			win.PostMessage(h, win.WM_KEYDOWN, k, 1)
			win.PostMessage(h, win.WM_KEYUP, k, 1|3<<30)
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

func TestDialogKeys(t *testing.T) {
	useManifest(t)
	setText := func(h win.HWND) {
		s, _ := syscall.UTF16PtrFromString("1234")
		win.SendMessage(h, win.WM_SETTEXT, 0, uintptr(unsafe.Pointer(s)))
	}

	typeAndPress("enter test", "Edit", setText, win.VK_RETURN)
	if got, ok := askText(nil, "enter test", "Eingabe:", false); !ok || got != "1234" {
		t.Errorf("Enter in askText: %q, %v; want \"1234\", true", got, ok)
	}

	typeAndPress("esc test", "Edit", setText, win.VK_ESCAPE)
	if _, ok := askText(nil, "esc test", "Eingabe:", false); ok {
		t.Error("Esc in askText did not cancel")
	}

	typeAndPress("choice test", "ListBox", nil, win.VK_DOWN, win.VK_RETURN)
	if got, ok := askChoice(nil, "choice test", "Welche?", []string{"a", "b", "c"}); !ok || got != 1 {
		t.Errorf("Down, Enter in askChoice: %d, %v; want 1, true", got, ok)
	}

	// Space on the focused OK button presses it.
	go func() {
		dlg := waitWindow("space test")
		ok := buttonWithText(dlg, "OK")
		win.PostMessage(dlg, win.WM_NEXTDLGCTL, uintptr(ok), 1)
		time.Sleep(100 * time.Millisecond)
		win.PostMessage(ok, win.WM_KEYDOWN, win.VK_SPACE, 1)
		win.PostMessage(ok, win.WM_KEYUP, win.VK_SPACE, 1|3<<30)
	}()
	if _, ok := askText(nil, "space test", "Eingabe:", false); !ok {
		t.Error("Space on OK did not accept")
	}
}

func TestWizardBackButtonByKeyboard(t *testing.T) {
	useManifest(t)
	withLang(t, "de")
	opened := make(chan *wizard, 1)
	wizardOpened = func(w *wizard) { opened <- w }
	defer func() { wizardOpened = nil }()
	errs := make(chan error, 1)
	go func() {
		w := <-opened
		errs <- func() error {
			defer w.dlg.Synchronize(func() { w.dlg.Cancel() })
			waitGone("BackupZit")
			onUI(w.dlg, func() { w.show(2) })
			onUI(w.dlg, func() { _ = w.back.pb.SetFocus() })
			press(w.dlg, w.back.pb.Handle(), win.VK_SPACE)
			var page int
			var nextText string
			onUI(w.dlg, func() { page, nextText = w.page, w.next.pb.Text() })
			if page != 1 {
				return fmt.Errorf("Space on Zurück: page %d, want 1", page)
			}
			onUI(w.dlg, func() { w.show(len(w.pages) - 2) })
			onUI(w.dlg, func() { nextText = w.next.pb.Text() })
			if nextText != "Wiederherstellen" || w.next.kind != btnDanger {
				return fmt.Errorf("summary page: next button %q kind %d", nextText, w.next.kind)
			}
			return nil
		}()
	}()
	runRestoreWizard(nil)
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
}
