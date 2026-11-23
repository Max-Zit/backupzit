package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/localipc"
	"github.com/max-zit/backupzit/internal/macperm"
)

// This file is the permission assistant's logic, kept free of system calls
// so it can be tested: what each permission's menu item says, and which
// guidance dialog to show when.

// permState is the state of one permission as the menu shows it.
type permState string

const (
	permGranted permState = "granted"
	permDenied  permState = "denied"
	permUnknown permState = "unknown"
	// permAsk: macOS has not asked the user yet (notifications).
	permAsk permState = "not-determined"
	// permAbsent: nothing to grant, e.g. no login item installed because the
	// agent was not installed from the .pkg.
	permAbsent permState = "not-installed"
)

// permKey names a permission.
type permKey string

const (
	keyAgent  permKey = "agent"         // the agent service may run in the background
	keyFDA    permKey = "fda"           // Full Disk Access of the agent
	keyNotify permKey = "notifications" // notifications of the menu bar app
	keyLogin  permKey = "login"         // the menu bar app opens at login
)

// permOrder is the order of the menu items.
var permOrder = []permKey{keyAgent, keyFDA, keyNotify, keyLogin}

// guideOrder is the order the assistant walks the user through: what
// backups need first. A stopped agent is not in it: the status line at the
// top of the menu already says so, and it is normal during an update.
var guideOrder = []permKey{keyFDA, keyLogin, keyNotify}

type permInfo struct {
	label string
	url   string // System Settings pane that grants it
	help  string // tooltip
}

var permInfos = map[permKey]permInfo{
	keyAgent: {"Agent allowed in the background", macperm.BackgroundItemsSettingsURL,
		"Login Items & Extensions > Allow in the Background: BackupZit must be on, or no backup runs."},
	keyFDA: {"Full Disk Access for the agent", macperm.FullDiskAccessSettingsURL,
		"Privacy & Security > Full Disk Access: without it macOS hides protected folders from backups."},
	keyNotify: {"Notifications", macperm.NotificationsSettingsURL,
		"Notifications > BackupZit: to be told when a backup fails."},
	keyLogin: {"Open at login", macperm.BackgroundItemsSettingsURL,
		"Login Items & Extensions > Allow in the Background: shows this menu after each login."},
}

// perms is one check of every permission.
type perms struct {
	state        map[permKey]permState
	agentVersion string
	agentPath    string // the agent program to switch on in Full Disk Access
}

// permsFrom combines what the agent reports (st is nil when it is not
// reachable) with what the menu bar app checks itself.
func permsFrom(st *localipc.Status, notify, login permState) perms {
	p := perms{state: map[permKey]permState{keyNotify: notify, keyLogin: login, keyAgent: permDenied, keyFDA: permUnknown}}
	if st == nil {
		return p
	}
	p.state[keyAgent] = permGranted
	p.agentVersion = st.Version
	if mp := st.MacPermissions; mp != nil {
		p.agentPath = mp.AgentPath
		switch macperm.State(mp.FullDiskAccess) {
		case macperm.Granted:
			p.state[keyFDA] = permGranted
		case macperm.Denied:
			p.state[keyFDA] = permDenied
		}
	}
	return p
}

// missing reports whether something is left for the user to grant, which
// makes the app check again every few seconds so the ✅ appears by itself.
func (p perms) missing() bool {
	for _, k := range permOrder {
		if s := p.state[k]; s == permDenied || s == permAsk {
			return true
		}
	}
	return false
}

// title is the menu item of permission k.
func (p perms) title(k permKey) string {
	label := permInfos[k].label
	switch p.state[k] {
	case permGranted:
		return "✅ " + label
	case permDenied:
		return "❌ " + label + " — click to fix"
	case permAsk:
		return "❌ " + label + " — not allowed yet"
	case permAbsent:
		return "➖ " + label + " (not installed)"
	}
	if k == keyFDA && p.state[keyAgent] != permGranted {
		return "❔ " + label + " (agent not reachable)"
	}
	return "❔ " + label + " (unknown)"
}

// header is the title of the menu's permission section.
func (p perms) header() string {
	n := 0
	for _, k := range permOrder {
		if s := p.state[k]; s == permDenied || s == permAsk {
			n++
		}
	}
	switch n {
	case 0:
		return "Permissions"
	case 1:
		return "Permissions — 1 missing"
	}
	return fmt.Sprintf("Permissions — %d missing", n)
}

// guideMemory is what the assistant remembers between launches (saved in
// the user's Application Support folder).
type guideMemory struct {
	// FDAGrantedVersion is the agent version that last had Full Disk
	// Access: if a later check finds it denied, the permission was lost
	// (typically by an update of an agent not signed by Apple, whose new
	// program macOS no longer recognizes).
	FDAGrantedVersion string `json:"fda_granted_version,omitempty"`
	// LossShownVersion is the agent version the loss was explained for, so
	// it is explained once per version and not at every check.
	LossShownVersion string `json:"loss_shown_version,omitempty"`
}

// waitStep is how long the assistant waits for the user to finish a step
// in System Settings before it proposes the next one anyway.
const waitStep = 5 * time.Minute

// guide decides which guidance dialog to show. It lives for one launch of
// the app: each missing permission is explained once per launch (so at each
// login while it is missing), "Later" silences the rest of the launch, and
// a lost Full Disk Access is explained even then.
type guide struct {
	mem          guideMemory
	shown        map[permKey]bool
	snoozed      bool // the user answered "Later"
	open         bool // a dialog is on screen
	waitingFor   permKey
	waitingSince time.Time
	askedNotify  bool
}

func newGuide(mem guideMemory) *guide {
	return &guide{mem: mem, shown: map[permKey]bool{}}
}

// dialog is a guidance dialog.
type dialog struct {
	key  permKey
	text string
	url  string
}

// next returns the dialog to show now (nil for none) and whether the
// memory changed and must be saved.
func (g *guide) next(p perms, now time.Time) (*dialog, bool) {
	changed := false
	fda := p.state[keyFDA]
	if fda == permGranted && p.agentVersion != "" && g.mem.FDAGrantedVersion != p.agentVersion {
		g.mem.FDAGrantedVersion, g.mem.LossShownVersion = p.agentVersion, ""
		changed = true
	}
	if g.open {
		return nil, changed
	}
	if fda == permDenied && g.mem.FDAGrantedVersion != "" && g.mem.LossShownVersion != p.agentVersion {
		before := g.mem.FDAGrantedVersion
		g.mem.LossShownVersion = p.agentVersion
		g.shown[keyFDA] = true
		g.open = true
		return &dialog{key: keyFDA, text: fdaLostText(p, before), url: permInfos[keyFDA].url}, true
	}
	if g.snoozed {
		return nil, changed
	}
	if g.waitingFor != "" {
		if p.state[g.waitingFor] != permGranted && now.Sub(g.waitingSince) < waitStep {
			return nil, changed
		}
		g.waitingFor = ""
	}
	for _, k := range guideOrder {
		if p.state[k] != permDenied || g.shown[k] {
			continue
		}
		g.shown[k] = true
		g.open = true
		return &dialog{key: k, text: guideText(k, p), url: permInfos[k].url}, changed
	}
	return nil, changed
}

// answered records the user's answer to d: openSettings for "Open System
// Settings" (the assistant then waits for that step), false for "Later" or
// a dialog left unanswered.
func (g *guide) answered(d *dialog, openSettings bool, now time.Time) {
	g.open = false
	if openSettings {
		g.waitingFor, g.waitingSince = d.key, now
		return
	}
	g.snoozed = true
}

// requestNotifications reports whether to ask macOS for the notification
// permission now: once per launch while macOS has not asked the user, and
// not over a guidance dialog.
func (g *guide) requestNotifications(p perms) bool {
	if p.state[keyNotify] != permAsk || g.askedNotify || g.open {
		return false
	}
	g.askedNotify = true
	return true
}

// agentName is how the guidance names the agent program to switch on.
func agentName(p perms) string {
	if p.agentPath == "" {
		return "“backupzit-agent”"
	}
	return "“" + p.agentPath + "”"
}

// fdaSteps are the Full Disk Access steps; re-adding is needed after an
// update of an unsigned agent, as the switch then refers to the old program.
func fdaSteps(p perms, readd bool) string {
	var b strings.Builder
	b.WriteString("1. Click “Open System Settings”.\n")
	if readd {
		b.WriteString("2. In Full Disk Access, select backupzit-agent and click − to remove it (switching it off and on is not enough).\n")
		b.WriteString("3. Click +, press ⌘⇧G, enter " + agentName(p) + " and click Open.\n")
		b.WriteString("4. Make sure its switch is on.")
		return b.String()
	}
	b.WriteString("2. In Full Disk Access, switch on backupzit-agent.\n")
	b.WriteString("   Not in the list? Click +, press ⌘⇧G, enter " + agentName(p) + " and click Open.\n")
	b.WriteString("3. The BackupZit menu shows ✅ once it is done.")
	return b.String()
}

// guideText is the text of the first-time guidance for k.
func guideText(k permKey, p perms) string {
	switch k {
	case keyFDA:
		return "BackupZit needs Full Disk Access: without it macOS hides your mail, messages, Safari data and other protected folders from backups.\n\n" + fdaSteps(p, false)
	case keyLogin:
		return "The BackupZit menu is switched off in Login Items, so it will not be there after your next login.\n\n" +
			"1. Click “Open System Settings”.\n2. Under “Allow in the Background”, switch on BackupZit."
	case keyNotify:
		return "BackupZit may not show notifications, so you would not be told when a backup fails.\n\n" +
			"1. Click “Open System Settings”.\n2. Select BackupZit in the list.\n3. Switch on “Allow notifications”."
	}
	return ""
}

// fdaLostText explains a Full Disk Access that was granted and is gone.
func fdaLostText(p perms, grantedVersion string) string {
	why := "macOS no longer gives Full Disk Access to the BackupZit agent"
	if grantedVersion != p.agentVersion && p.agentVersion != "" {
		why += " since its update to version " + p.agentVersion
	}
	return why + ", so protected folders are left out of backups. This happens when an update is not signed by Apple.\n\n" + fdaSteps(p, true)
}

// loginItemState tells the state of the menu bar app's login item from the
// LaunchAgent installed by the .pkg (plistExists), whether launchd started
// this process for it, and whether launchd has it loaded for this user.
// macOS does not load a LaunchAgent switched off in Login Items.
func loginItemState(plistExists, startedByLaunchd, loaded bool) permState {
	switch {
	case !plistExists:
		return permAbsent
	case startedByLaunchd || loaded:
		return permGranted
	}
	return permDenied
}

// Notification authorization as notifyAuthorization reports it.
const (
	notifyUnknown       = 0 // not an app bundle, or no answer
	notifyNotDetermined = 1
	notifyDenied        = 2
	notifyAllowed       = 3
)

func notifyState(code int) permState {
	switch code {
	case notifyNotDetermined:
		return permAsk
	case notifyDenied:
		return permDenied
	case notifyAllowed:
		return permGranted
	}
	return permUnknown
}

// dialogButtonOpen is the dialog's button that opens System Settings.
const dialogButtonOpen = "Open System Settings"

// openChosen reads osascript's "display dialog" result: true only when the
// user clicked dialogButtonOpen (not "Later", not left unanswered).
func openChosen(out string) bool {
	return strings.Contains(out, "button returned:"+dialogButtonOpen) && !strings.Contains(out, "gave up:true")
}
