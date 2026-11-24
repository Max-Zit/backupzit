//go:build windows

package main

import (
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/lxn/win"
)

var (
	user32               = syscall.NewLazyDLL("user32.dll")
	procEnumChildWindows = user32.NewProc("EnumChildWindows")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateActCtx     = kernel32.NewProc("CreateActCtxW")
	procActivateActCtx   = kernel32.NewProc("ActivateActCtx")
)

// useManifest locks the test to its OS thread and activates the
// program's manifest (common controls v6) there, which the real executable
// gets from the file next to it; walk needs it.
func useManifest(t *testing.T) {
	t.Helper()
	// These tests open real windows and answer them by window title. On a
	// workstation they pop up on the user's desktop, and two test runs at
	// the same time answer each other's dialogs and leave one waiting.
	if os.Getenv("BACKUPZIT_UI_TESTS") == "" {
		t.Skip("opens real dialogs on the desktop; set BACKUPZIT_UI_TESTS=1 to run it (lab VM or CI)")
	}
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)
	src, _ := syscall.UTF16PtrFromString("backupzit-recovery.exe.manifest")
	ctx := struct {
		size, flags           uint32
		source                *uint16
		arch                  uint16
		lang                  uint16
		dir, resName, appName *uint16
		module                uintptr
	}{source: src}
	ctx.size = uint32(unsafe.Sizeof(ctx))
	h, _, err := procCreateActCtx.Call(uintptr(unsafe.Pointer(&ctx)))
	if h == ^uintptr(0) {
		t.Fatalf("CreateActCtx: %v", err)
	}
	var cookie uintptr
	if r, _, err := procActivateActCtx.Call(h, uintptr(unsafe.Pointer(&cookie))); r == 0 {
		t.Fatalf("ActivateActCtx: %v", err)
	}
}

// childOfClass returns the first descendant of parent with the class.
func childOfClass(parent win.HWND, class string) win.HWND {
	var found win.HWND
	cb := syscall.NewCallback(func(h win.HWND, _ uintptr) uintptr {
		buf := make([]uint16, 64)
		win.GetClassName(h, &buf[0], len(buf))
		if syscall.UTF16ToString(buf) == class {
			found = h
			return 0
		}
		return 1
	})
	_, _, _ = procEnumChildWindows.Call(uintptr(parent), cb, 0)
	return found
}

// answerDialog waits for the dialog with the title, lets prepare change
// its controls and presses OK (Enter).
func answerDialog(t *testing.T, title string, prepare func(dlg win.HWND)) {
	t.Helper()
	go func() {
		name, _ := syscall.UTF16PtrFromString(title)
		for i := 0; i < 200; i++ {
			if h := win.FindWindow(nil, name); h != 0 && win.IsWindowVisible(h) {
				prepare(h)
				win.PostMessage(h, win.WM_COMMAND, uintptr(win.IDOK), 0)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
}

func TestAskTextReturnsTypedText(t *testing.T) {
	useManifest(t)
	answerDialog(t, "askText test", func(dlg win.HWND) {
		edit := childOfClass(dlg, "Edit")
		if edit == 0 {
			return
		}
		s, _ := syscall.UTF16PtrFromString("riso-zugang-2026")
		win.SendMessage(edit, win.WM_SETTEXT, 0, uintptr(unsafe.Pointer(s)))
	})
	got, ok := askText(nil, "askText test", "Passphrase:", true)
	if !ok || got != "riso-zugang-2026" {
		t.Fatalf("askText = %q, %v; want the typed text", got, ok)
	}
}

func TestAskChoiceReturnsSelectedItem(t *testing.T) {
	useManifest(t)
	answerDialog(t, "askChoice test", func(dlg win.HWND) {
		if lb := childOfClass(dlg, "ListBox"); lb != 0 {
			win.SendMessage(lb, win.LB_SETCURSEL, 2, 0)
		}
	})
	got, ok := askChoice(nil, "askChoice test", "Which?", []string{"a", "b", "c"})
	if !ok || got != 2 {
		t.Fatalf("askChoice = %d, %v; want 2, true", got, ok)
	}
}

// closeDialog waits for the dialog with the title and closes it.
func closeDialog(title string) {
	go func() {
		name, _ := syscall.UTF16PtrFromString(title)
		for i := 0; i < 200; i++ {
			if h := win.FindWindow(nil, name); h != 0 && win.IsWindowVisible(h) {
				win.PostMessage(h, win.WM_CLOSE, 0, 0)
				return
			}
			time.Sleep(25 * time.Millisecond)
		}
	}()
}

func TestNetworkDialogOpensWithoutAdapters(t *testing.T) {
	useManifest(t)
	withLang(t, "en")
	closeDialog("Network")
	if err := networkDialog(nil, nil); err != nil {
		t.Fatalf("no network adapter: %v", err)
	}
}
