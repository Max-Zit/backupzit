package main

// serviceHint tells what to do when the agent service does not answer. On
// macOS the usual cause is the user switching BackupZit off in Login Items,
// which stops the launchd daemon too.
const serviceHint = "Check System Settings > General > Login Items & Extensions: BackupZit must be allowed in the background. Otherwise reinstall the agent."

// enrollHint and enrollRetryHint tell how to enroll a Mac that is not
// enrolled yet: from this app's menu (enroll_darwin.go), since a .pkg cannot
// ask for the code.
const (
	enrollHint      = "Choose Enroll this Mac… in this menu and paste the enrollment code from the console (Agents page)."
	enrollRetryHint = "Create a new enrollment code on the console's agent port (usually :8443) and choose Enroll this Mac… in this menu."
)
