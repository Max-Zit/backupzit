package main

// serviceHint tells what to do when the agent service does not answer. On
// macOS the usual cause is the user switching BackupZit off in Login Items,
// which stops the launchd daemon too.
const serviceHint = "Check System Settings > General > Login Items & Extensions: BackupZit must be allowed in the background. Otherwise reinstall the agent."
