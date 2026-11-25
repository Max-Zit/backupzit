//go:build windows

package main

// Wizard pages and dialogs scroll on a small screen (see compact), the
// partition rows when there are many. Windows does not scroll a view to
// the control that gets the keyboard focus, so Tab could move to a control
// below the visible part. revealFocus watches the focus in this program and
// scrolls every view that holds the focused control until the control shows.

import (
	"math"
	"syscall"
	"unsafe"

	"github.com/lxn/win"
	"golang.org/x/sys/windows"
)

var (
	userDLL             = windows.NewLazySystemDLL("user32.dll")
	procSetWinEventHook = userDLL.NewProc("SetWinEventHook")
)

const (
	eventObjectFocus = 0x8005 // EVENT_OBJECT_FOCUS
	// winEventOutOfContext delivers the events through the message loop of
	// the thread that set the hook: the window thread.
	winEventOutOfContext = 0
	scrollViewClass      = `\o/ Walk_ScrollView_Class \o/`
)

var focusHook uintptr

// revealFocus starts watching the focus. Call it on the window thread.
func revealFocus() {
	if focusHook != 0 {
		return
	}
	cb := syscall.NewCallback(func(_, _, hwnd, _, _, _, _ uintptr) uintptr {
		scrollIntoView(win.HWND(hwnd))
		return 0
	})
	focusHook, _, _ = procSetWinEventHook.Call(eventObjectFocus, eventObjectFocus, 0, cb,
		uintptr(windows.GetCurrentProcessId()), uintptr(windows.GetCurrentThreadId()), winEventOutOfContext)
}

// scrollIntoView scrolls each view that holds h, the innermost first.
func scrollIntoView(h win.HWND) {
	if h == 0 {
		return
	}
	for p := win.GetParent(h); p != 0; p = win.GetParent(p) {
		if className(p) == scrollViewClass {
			revealIn(p, h)
		}
	}
}

// revealIn scrolls the view sv up or down so that h shows, with a small
// gap; a control taller than the view shows its top.
func revealIn(sv, h win.HWND) {
	var r, view win.RECT
	if !win.GetWindowRect(h, &r) || !win.GetWindowRect(sv, &view) {
		return
	}
	var si win.SCROLLINFO
	si.CbSize = uint32(unsafe.Sizeof(si))
	si.FMask = win.SIF_POS | win.SIF_PAGE | win.SIF_RANGE
	if !win.GetScrollInfo(sv, win.SB_VERT, &si) || si.NPage == 0 {
		return
	}
	const gap = 8
	top, bottom, page := int64(r.Top-view.Top), int64(r.Bottom-view.Top), int64(si.NPage)
	pos := int64(si.NPos)
	switch {
	case top < 0 || bottom-top > page:
		pos += top - gap
	case bottom > page:
		pos += bottom - page + gap
	default:
		return
	}
	pos = max(pos, 0)
	if pos > math.MaxInt32 {
		pos = math.MaxInt32
	}
	si.FMask = win.SIF_POS
	si.NPos = int32(pos)
	win.SetScrollInfo(sv, win.SB_VERT, &si, true)
	// walk moves the content to the scroll bar's position on this message.
	win.SendMessage(sv, win.WM_VSCROLL, win.SB_THUMBPOSITION, 0)
}

func className(h win.HWND) string {
	buf := make([]uint16, 64)
	n, _ := win.GetClassName(h, &buf[0], len(buf))
	return syscall.UTF16ToString(buf[:max(n, 0)])
}
