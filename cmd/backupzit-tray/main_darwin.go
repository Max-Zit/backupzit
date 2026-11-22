// Command backupzit-tray shows the BackupZit agent in the macOS menu bar:
// its state as a template icon (tinted by macOS for light and dark menu
// bars), a menu with "Back up now", the console, and the permission
// assistant that guides the user through the privacy permissions only he
// can grant. It talks to the agent service over the local socket and runs
// once per signed-in user, started at login by a LaunchAgent.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/max-zit/backupzit/internal/localipc"
)

var (
	//go:embed icons/template-ok.png
	icoOK []byte
	//go:embed icons/template-running.png
	icoRunning []byte
	//go:embed icons/template-warning.png
	icoWarning []byte
	//go:embed icons/template-error.png
	icoError []byte
	//go:embed icons/template-offline.png
	icoOffline []byte
)

var stateIcon = map[string][]byte{"ok": icoOK, "running": icoRunning, "warning": icoWarning, "error": icoError, "offline": icoOffline, "idle": icoOK}

const maxJobItems = 12

// permRecheck is how often the permissions the app checks itself are
// checked once everything is granted (every status refresh while something
// is missing, so ✅ appears by itself).
const permRecheck = time.Minute

type tray struct {
	mu   sync.Mutex
	sum  Summary
	icon string

	status   *systray.MenuItem
	detail   *systray.MenuItem
	backup   *systray.MenuItem
	jobItems []*systray.MenuItem
	jobIDs   []int64
	console  *systray.MenuItem
	permHead *systray.MenuItem
	permItem map[permKey]*systray.MenuItem
	hide     *systray.MenuItem

	// Permission assistant.
	guide    *guide
	perm     perms
	notify   permState
	login    permState
	permAt   time.Time
	permDone bool // a first check was made
}

func main() {
	lock, err := lockInstance()
	if errors.Is(err, errRunning) {
		return
	}
	if err != nil {
		log.Printf("backupzit-tray: single instance lock: %v", err)
	}
	defer func() {
		if lock != nil {
			_ = lock.Close()
		}
	}()
	t := &tray{guide: newGuide(loadGuideMemory()), permItem: map[permKey]*systray.MenuItem{}}
	systray.Run(t.ready, func() {})
}

func (t *tray) ready() {
	systray.SetTemplateIcon(icoOffline, icoOffline)
	systray.SetTooltip("BackupZit")
	t.status = systray.AddMenuItem("BackupZit", "")
	t.status.Disable()
	t.detail = systray.AddMenuItem("", "")
	t.detail.Disable()
	t.detail.Hide()
	systray.AddSeparator()
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
	t.addPermissionItems()
	systray.AddSeparator()
	t.hide = systray.AddMenuItem("Hide this icon", "The icon returns at the next login; backups continue in the background")

	go func() {
		for {
			select {
			case <-t.console.ClickedCh:
				t.openConsole()
			case <-t.hide.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
	go t.loop()
}

// addPermissionItems adds the permission section: a header, one item per
// permission that opens its System Settings pane, and a short explanation.
func (t *tray) addPermissionItems() {
	t.permHead = systray.AddMenuItem("Permissions", "")
	t.permHead.Disable()
	for _, k := range permOrder {
		it := systray.AddMenuItem(t.perm.title(k), permInfos[k].help)
		t.permItem[k] = it
		go func(url string, it *systray.MenuItem) {
			for range it.ClickedCh {
				openSettings(url)
			}
		}(permInfos[k].url, it)
	}
	why := systray.AddMenuItem("Granted by you in System Settings — click an item to go there", "")
	why.Disable()
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
		systray.SetTemplateIcon(stateIcon[sum.State], stateIcon[sum.State])
		t.icon = sum.State
	}
	systray.SetTooltip(sum.tooltip())
	head := sum.Headline
	if sum.State == "running" && sum.Percent >= 0 {
		head = fmt.Sprintf("%s — %d%%", head, sum.Percent)
	}
	t.status.SetTitle(head)
	t.status.SetTooltip(sum.Detail)
	if sum.Status != nil {
		t.detail.SetTitle("Last: " + sum.LastText + "   Next: " + sum.NextText)
	} else {
		t.detail.SetTitle(sum.Detail)
	}
	t.detail.Show()
	t.updateJobs(sum)
	if sum.Status == nil || sum.Status.ServerURL == "" {
		t.console.Disable()
	} else {
		t.console.Enable()
	}
	return sum
}

// updateJobs fills the "Back up now" submenu; t.mu is held.
func (t *tray) updateJobs(sum Summary) {
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
}

// checkPermissions updates the permission section from the agent's status
// and the app's own checks, and starts the guidance the assistant decides.
func (t *tray) checkPermissions(sum Summary) {
	t.mu.Lock()
	recheck := !t.permDone || t.perm.missing() || time.Since(t.permAt) >= permRecheck
	t.mu.Unlock()
	if recheck {
		// Outside the lock: launchctl and the notification center may take
		// a moment.
		notify, login := checkNotifications(), checkLoginItem(context.Background())
		t.mu.Lock()
		t.notify, t.login, t.permAt, t.permDone = notify, login, time.Now(), true
		t.mu.Unlock()
	}

	t.mu.Lock()
	t.perm = permsFrom(sum.Status, t.notify, t.login)
	p := t.perm
	t.permHead.SetTitle(p.header())
	for _, k := range permOrder {
		t.permItem[k].SetTitle(p.title(k))
	}
	askNotify := t.guide.requestNotifications(p)
	d, changed := t.guide.next(p, time.Now())
	mem := t.guide.mem
	t.mu.Unlock()

	if changed {
		if err := saveGuideMemory(mem); err != nil {
			log.Printf("backupzit-tray: save assistant state: %v", err)
		}
	}
	if askNotify {
		requestNotificationPermission()
	}
	if d != nil {
		go t.showGuidance(d)
	}
}

// showGuidance shows one guidance dialog and records the answer.
func (t *tray) showGuidance(d *dialog) {
	open := showDialog(context.Background(), d.text)
	if open {
		openSettings(d.url)
	}
	t.mu.Lock()
	t.guide.answered(d, open, time.Now())
	t.mu.Unlock()
}

func (t *tray) loop() {
	for {
		sum := t.refresh()
		t.checkPermissions(sum)
		every := 10 * time.Second
		if sum.State == "running" {
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

// runJob starts a job now; an error is shown in the tooltip.
func (t *tray) runJob(id int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdRun, JobID: id}); err != nil {
		systray.SetTooltip("BackupZit — " + strings.TrimSpace(err.Error()))
		return
	}
	go func() {
		time.Sleep(time.Second)
		t.refresh()
	}()
}

func (t *tray) openConsole() {
	s := t.summary()
	if s.Status == nil || s.Status.ServerURL == "" {
		return
	}
	openURL(s.Status.ServerURL)
}
