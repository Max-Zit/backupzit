package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"unsafe"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed ui.html
var uiHTML string

//go:embed icons/app.ico
var appIcon []byte

// statusWindow is the "Open BackupZit Agent" window. It is rendered with
// WebView2 (present on Windows 10/11); without it the console opens in the
// browser instead.
type statusWindow struct {
	mu   sync.Mutex
	w    webview2.WebView
	hwnd uintptr
}

func newStatusWindow() *statusWindow { return &statusWindow{} }

func (s *statusWindow) isOpen() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w != nil
}

var (
	user32              = windows.NewLazySystemDLL("user32.dll")
	procSetForeground   = user32.NewProc("SetForegroundWindow")
	procShowWindow      = user32.NewProc("ShowWindow")
	procSendMessage     = user32.NewProc("SendMessageW")
	procLoadImage       = user32.NewProc("LoadImageW")
	procPostMessage     = user32.NewProc("PostMessageW")
	procIsIconic        = user32.NewProc("IsIconic")
)

func (s *statusWindow) show(t *tray) {
	s.mu.Lock()
	if s.w != nil {
		h := s.hwnd
		s.mu.Unlock()
		if r, _, _ := procIsIconic.Call(h); r != 0 {
			procShowWindow.Call(h, 9) // SW_RESTORE
		}
		procSetForeground.Call(h)
		return
	}
	s.mu.Unlock()
	ready := make(chan bool)
	go s.run(t, ready)
	if !<-ready {
		t.openConsole() // no WebView2 runtime
	}
}

func (s *statusWindow) run(t *tray, ready chan<- bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	dataDir := filepath.Join(os.Getenv("LOCALAPPDATA"), "BackupZit", "TrayWebView")
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		DataPath:  dataDir,
		AutoFocus: true,
		WindowOptions: webview2.WindowOptions{
			Title: "BackupZit Agent", Width: 860, Height: 620, Center: true,
		},
	})
	if w == nil {
		ready <- false
		return
	}
	hwnd := uintptr(w.Window())
	setWindowIcon(hwnd)
	w.SetSize(720, 480, webview2.HintMin)
	w.Bind("bzStatus", func() Summary { return t.summary() })
	w.Bind("bzRun", func(id float64) string { return t.runJob(int64(id)) })
	w.Bind("bzConsole", func() { t.openConsole() })
	w.Bind("bzVersion", func() string { return version })
	w.SetHtml(uiHTML)
	s.mu.Lock()
	s.w, s.hwnd = w, hwnd
	s.mu.Unlock()
	ready <- true
	w.Run()
	s.mu.Lock()
	s.w, s.hwnd = nil, 0
	s.mu.Unlock()
	w.Destroy()
}

// update pushes a new summary to the open window.
func (s *statusWindow) update(sum Summary) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	if w == nil {
		return
	}
	b, _ := json.Marshal(sum)
	w.Dispatch(func() { w.Eval("window.bzUpdate && window.bzUpdate(" + string(b) + ")") })
}

func (s *statusWindow) close() {
	s.mu.Lock()
	h := s.hwnd
	s.mu.Unlock()
	if h != 0 {
		procPostMessage.Call(h, 0x0010, 0, 0) // WM_CLOSE
	}
}

// setWindowIcon loads the BackupZit icon for the window title bar and the
// taskbar.
func setWindowIcon(hwnd uintptr) {
	dir := filepath.Join(os.Getenv("LOCALAPPDATA"), "BackupZit")
	os.MkdirAll(dir, 0o700)
	p := filepath.Join(dir, "backupzit.ico")
	if err := os.WriteFile(p, appIcon, 0o600); err != nil {
		return
	}
	path, _ := windows.UTF16PtrFromString(p)
	const imageIcon, lrLoadFromFile, wmSetIcon = 1, 0x10, 0x0080
	for i, size := range []uintptr{16, 32} {
		h, _, _ := procLoadImage.Call(0, uintptr(unsafe.Pointer(path)), imageIcon, size, size, lrLoadFromFile)
		if h != 0 {
			procSendMessage.Call(hwnd, wmSetIcon, uintptr(i), h) // ICON_SMALL for 16, ICON_BIG for 32
		}
	}
}
