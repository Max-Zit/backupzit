// Package macperm checks the macOS privacy permissions the agent needs and
// that only the user can grant (System Settings > Privacy & Security), so
// the agent can report a missing one and the tray app can guide the user to
// it. On other systems every check answers NotApplicable.
package macperm

// State is the state of one permission.
type State string

const (
	Granted       State = "granted"
	Denied        State = "denied"
	Unknown       State = "unknown"        // could not be determined
	NotApplicable State = "not-applicable" // not macOS
)

// FullDiskAccessSettingsURL opens System Settings at Privacy & Security >
// Full Disk Access.
const FullDiskAccessSettingsURL = "x-apple.systempreferences:com.apple.preference.security?Privacy_AllFiles"

// BackgroundItemsSettingsURL opens System Settings at General > Login Items
// & Extensions, where a background service can be switched off.
const BackgroundItemsSettingsURL = "x-apple.systempreferences:com.apple.LoginItems-Settings.extension"

// NotificationsSettingsURL opens System Settings at Notifications.
const NotificationsSettingsURL = "x-apple.systempreferences:com.apple.Notifications-Settings.extension"

// FullDiskAccess reports whether this process may read the folders macOS
// protects (Mail, Messages, Safari, other apps' data, Time Machine): without
// Full Disk Access a backup silently misses them.
func FullDiskAccess() State { return fullDiskAccess() }
