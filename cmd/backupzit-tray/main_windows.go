// Command backupzit-tray shows the BackupZit agent in the notification area
// (next to the clock): its state as icon colour, a menu with "Back up now"
// and a status window. It talks to the agent service over a local pipe and
// runs once per logged-on user.
package main

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/max-zit/backupzit/internal/localipc"
	"golang.org/x/sys/windows"
)

var (
	//go:embed icons/tray-ok.ico
	icoOK []byte
	//go:embed icons/tray-running.ico
	icoRunning []byte
	//go:embed icons/tray-warning.ico
	icoWarning []byte
	//go:embed icons/tray-error.ico
	icoError []byte
	//go:embed icons/tray-offline.ico
	icoOffline []byte
)

var stateIcon = map[string][]byte{"ok": icoOK, "running": icoRunning, "warning": icoWarning, "error": icoError, "offline": icoOffline, "idle": icoOK}

var version = "dev"

const maxJobItems = 12

type tray struct {
	mu      sync.Mutex
	sum     Summary
	icon    string
	lastRun int64

	status   *systray.MenuItem
	detail   *systray.MenuItem
	open     *systray.MenuItem
	backup   *systray.MenuItem
	jobItems []*systray.MenuItem
	jobIDs   []int64
	console  *systray.MenuItem
	hide     *systray.MenuItem
	win      *statusWindow
}

func main() {
	show := len(os.Args) > 1 && (os.Args[1] == "--show" || os.Args[1] == "/show")
	if isServiceSession() {
		return // never in session 0 (e.g. started by an installer running as SYSTEM)
	}
	// One instance per session; a second start opens the window of the first.
	showEvent, _ := windows.CreateEvent(nil, 0, 0, windows.StringToUTF16Ptr(`Local\BackupZitTrayShow`))
	m, err := windows.CreateMutex(nil, false, windows.StringToUTF16Ptr(`Local\BackupZitTray`))
	if err == windows.ERROR_ALREADY_EXISTS {
		windows.SetEvent(showEvent)
		return
	}
	defer windows.CloseHandle(m)

	t := &tray{win: newStatusWindow()}
	systray.Run(func() { t.ready(showEvent, show) }, func() {})
}

// isServiceSession reports whether this process runs in session 0.
func isServiceSession() bool {
	var sid uint32
	if err := windows.ProcessIdToSessionId(windows.GetCurrentProcessId(), &sid); err != nil {
		return false
	}
	return sid == 0
}

func (t *tray) ready(showEvent windows.Handle, show bool) {
	systray.SetIcon(icoOffline)
	systray.SetTitle("BackupZit")
	systray.SetTooltip("BackupZit")
	t.status = systray.AddMenuItem("BackupZit", "")
	t.status.Disable()
	t.detail = systray.AddMenuItem("", "")
	t.detail.Disable()
	systray.AddSeparator()
	t.open = systray.AddMenuItem("Open BackupZit Agent", "Show the status of this computer's backups")
	t.backup = systray.AddMenuItem("Back up now", "Start a backup job now")
	for i := 0; i < maxJobItems; i++ {
		it := t.backup.AddSubMenuItem("", "")
		it.Hide()
		t.jobItems = append(t.jobItems, it)
		t.jobIDs = append(t.jobIDs, 0)
		go func(i int, it *systray.MenuItem) {
			for range it.ClickedCh {
				t.mu.Lock()
				id := t.jobIDs[i]
				t.mu.Unlock()
				if id != 0 {
					go t.runJob(id)
				}
			}
		}(i, it)
	}
	t.console = systray.AddMenuItem("Open console in browser", "Open the BackupZit management console")
	systray.AddSeparator()
	t.hide = systray.AddMenuItem("Hide this icon", "The icon returns at the next sign-in; backups continue in the background")

	systray.SetOnTapped(func() { t.win.show(t) })
	go func() {
		for {
			select {
			case <-t.open.ClickedCh:
				t.win.show(t)
			case <-t.console.ClickedCh:
				t.openConsole()
			case <-t.hide.ClickedCh:
				t.win.close()
				systray.Quit()
				return
			}
		}
	}()
	if showEvent != 0 {
		go func() {
			for {
				if ev, _ := windows.WaitForSingleObject(showEvent, windows.INFINITE); ev == windows.WAIT_OBJECT_0 {
					t.win.show(t)
				}
			}
		}()
	}
	go t.loop()
	if show {
		t.win.show(t)
	}
}

// refresh asks the agent for its status and updates icon and menu.
func (t *tray) refresh() Summary {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdStatus})
	var st *localipc.Status
	if err == nil {
		st = resp.Status
	}
	sum := summarize(st, err, time.Now())

	t.mu.Lock()
	defer t.mu.Unlock()
	t.sum = sum
	if sum.State != t.icon {
		systray.SetIcon(stateIcon[sum.State])
		t.icon = sum.State
	}
	systray.SetTooltip(sum.tooltip())
	head := sum.Headline
	if sum.State == "running" && sum.Percent >= 0 {
		head = fmt.Sprintf("%s — %d%%", head, sum.Percent)
	}
	t.status.SetTitle(head)
	if sum.Status != nil {
		t.detail.SetTitle("Last: " + sum.LastText + "   Next: " + sum.NextText)
		t.detail.Show()
	} else {
		t.detail.Hide()
	}
	n := 0
	for _, j := range sum.Jobs {
		if n == maxJobItems {
			break
		}
		title := j.Name
		if j.Running {
			title += "  (running)"
		}
		t.jobItems[n].SetTitle(title)
		t.jobItems[n].SetTooltip(j.KindText + " · last " + j.LastText + " · next " + j.NextText)
		if j.Running {
			t.jobItems[n].Disable()
		} else {
			t.jobItems[n].Enable()
		}
		t.jobItems[n].Show()
		t.jobIDs[n] = j.ID
		n++
	}
	for i := n; i < maxJobItems; i++ {
		t.jobItems[i].Hide()
		t.jobIDs[i] = 0
	}
	if n == 0 {
		t.backup.Disable()
	} else {
		t.backup.Enable()
	}
	if sum.Status == nil || sum.Status.ServerURL == "" {
		t.console.Disable()
	} else {
		t.console.Enable()
	}
	return sum
}

func (t *tray) loop() {
	for {
		sum := t.refresh()
		t.win.update(sum)
		every := 10 * time.Second
		if sum.State == "running" || t.win.isOpen() {
			every = 2 * time.Second
		}
		time.Sleep(every)
	}
}

func (t *tray) summary() Summary {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sum
}

// runJob starts a job now; errors are shown in the window and tooltip.
func (t *tray) runJob(id int64) string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdRun, JobID: id}); err != nil {
		msg := err.Error()
		systray.SetTooltip("BackupZit — " + strings.TrimSpace(msg))
		return msg
	}
	go func() {
		time.Sleep(time.Second)
		t.win.update(t.refresh())
	}()
	return ""
}

func (t *tray) openConsole() {
	s := t.summary()
	if s.Status == nil || s.Status.ServerURL == "" {
		return
	}
	openURL(s.Status.ServerURL)
}

func openURL(u string) {
	if !strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://") {
		return
	}
	windows.ShellExecute(0, windows.StringToUTF16Ptr("open"), windows.StringToUTF16Ptr(u), nil, nil, windows.SW_SHOWNORMAL)
}
