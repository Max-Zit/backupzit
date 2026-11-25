//go:build windows

package main

import (
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/walk"
	"github.com/lxn/win"
)

// TestScreenshots saves a picture of every window of the program to
// $BZ_SCREENSHOT_DIR, to look at the design without booting Windows PE.
var procPrintWindow = user32.NewProc("PrintWindow")

func TestScreenshots(t *testing.T) {
	dir := os.Getenv("BZ_SCREENSHOT_DIR")
	if dir == "" {
		t.Skip("set BZ_SCREENSHOT_DIR to save pictures of the windows")
	}
	useManifest(t)
	withLang(t, "de")

	a, err := newApp()
	if err != nil {
		t.Fatal(err)
	}
	shots := make(chan error, 1)
	go func() {
		shots <- func() error {
			mainWnd := waitWindow("BackupZit Recovery " + version)
			if mainWnd == 0 {
				return fmt.Errorf("main window not found")
			}
			if err := saveWindow(mainWnd, filepath.Join(dir, "01-hauptfenster.png")); err != nil {
				return err
			}
			for _, d := range []struct {
				title, file string
				open        func()
			}{
				{"Boot reparieren", "03-boot.png", func() { runBootRepair(a.mw) }},
				{"Treiber", "04-treiber.png", func() { runDrivers(a.mw) }},
				{"Netzwerk", "05-netzwerk.png", func() { runNetwork(a.mw) }},
				{"Eingabe", "06-eingabe.png", func() { askText(a.mw, "Eingabe", "Wiederherstellungsschlüssel (48 Ziffern):", false) }},
				{"Auswahl", "07-auswahl.png", func() {
					askChoice(a.mw, "Auswahl", "Welches Laufwerk entsperren?", []string{"Disk 1, Partition 3, 118 GB", "Disk 2, Partition 1, 931 GB"})
				}},
			} {
				a.mw.Synchronize(d.open)
				h := waitWindow(d.title)
				if h == 0 {
					return fmt.Errorf("%s not found", d.title)
				}
				waitGone("BackupZit")
				if err := saveWindow(h, filepath.Join(dir, d.file)); err != nil {
					return err
				}
				win.PostMessage(h, win.WM_CLOSE, 0, 0)
				time.Sleep(300 * time.Millisecond)
			}

			opened := make(chan *wizard, 1)
			wizardOpened = func(w *wizard) { opened <- w }
			defer func() { wizardOpened = nil }()
			a.mw.Synchronize(func() { runRestoreWizard(a.mw) })
			w := <-opened
			h := waitWindow("Image wiederherstellen")
			waitGone("BackupZit")
			for i := range w.pages {
				w.dlg.Synchronize(func() { w.show(i) })
				idle(w.dlg)
				if err := saveWindow(h, filepath.Join(dir, fmt.Sprintf("02-assistent-%d.png", i+1))); err != nil {
					return err
				}
			}
			win.PostMessage(h, win.WM_CLOSE, 0, 0)
			time.Sleep(300 * time.Millisecond)
			win.PostMessage(mainWnd, win.WM_CLOSE, 0, 0)
			return nil
		}()
	}()
	a.mw.Run()
	if err := <-shots; err != nil {
		t.Fatal(err)
	}
}

// idle waits until the window's thread has handled everything queued so far.
func idle(f walk.Form) {
	for range 3 {
		done := make(chan struct{})
		f.Synchronize(func() { close(done) })
		<-done
		time.Sleep(300 * time.Millisecond)
	}
}

func waitWindow(title string) win.HWND {
	name, _ := syscall.UTF16PtrFromString(title)
	for i := 0; i < 400; i++ {
		if h := win.FindWindow(nil, name); h != 0 && win.IsWindowVisible(h) {
			time.Sleep(700 * time.Millisecond)
			return h
		}
		time.Sleep(25 * time.Millisecond)
	}
	return 0
}

// waitGone waits until the busy window with the title has closed.
func waitGone(title string) {
	name, _ := syscall.UTF16PtrFromString(title)
	for i := 0; i < 1200; i++ {
		if h := win.FindWindow(nil, name); h == 0 || !win.IsWindowVisible(h) {
			time.Sleep(400 * time.Millisecond)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// saveWindow renders the window with PrintWindow and saves it as PNG.
func saveWindow(h win.HWND, file string) error {
	time.Sleep(1500 * time.Millisecond)
	var r win.RECT
	win.GetWindowRect(h, &r)
	wd, ht := r.Right-r.Left, r.Bottom-r.Top
	screen := win.GetDC(0)
	defer win.ReleaseDC(0, screen)
	mem := win.CreateCompatibleDC(screen)
	defer win.DeleteDC(mem)
	bmp := win.CreateCompatibleBitmap(screen, wd, ht)
	defer win.DeleteObject(win.HGDIOBJ(bmp))
	old := win.SelectObject(mem, win.HGDIOBJ(bmp))
	// Paint everything now: with the screen locked, the picture Windows
	// keeps of the window is not updated on its own.
	win.RedrawWindow(h, nil, 0, win.RDW_INVALIDATE|win.RDW_ERASE|win.RDW_ALLCHILDREN|win.RDW_UPDATENOW)
	const pwRenderFullContent = 2
	if ok, _, err := procPrintWindow.Call(uintptr(h), uintptr(mem), pwRenderFullContent); ok == 0 {
		return fmt.Errorf("PrintWindow: %v", err)
	}
	win.SelectObject(mem, old)

	var bi win.BITMAPINFO
	bi.BmiHeader.BiSize = uint32(unsafe.Sizeof(bi.BmiHeader))
	bi.BmiHeader.BiWidth = wd
	bi.BmiHeader.BiHeight = -ht
	bi.BmiHeader.BiPlanes = 1
	bi.BmiHeader.BiBitCount = 32
	bi.BmiHeader.BiCompression = win.BI_RGB
	px := make([]byte, int(wd)*int(ht)*4)
	if win.GetDIBits(mem, bmp, 0, uint32(ht), &px[0], &bi, win.DIB_RGB_COLORS) == 0 {
		return fmt.Errorf("GetDIBits failed")
	}
	img := image.NewRGBA(image.Rect(0, 0, int(wd), int(ht)))
	for i := 0; i < len(px); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = px[i+2], px[i+1], px[i], 255
	}
	f, err := os.Create(file) // #nosec G304 -- test output folder
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}
