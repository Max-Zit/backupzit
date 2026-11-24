//go:build !darwin

package main

// serviceHint tells what to do when the agent service does not answer.
const serviceHint = "Start the \"BackupZit Agent\" service or reinstall the agent."

// enrollHint and enrollRetryHint tell how to enroll a computer that is not
// enrolled yet: the Windows installer asks for the code.
const (
	enrollHint      = "Run the installer again and paste the enrollment code from the console (Agents page)."
	enrollRetryHint = "Create a new enrollment code on the console's agent port (usually :8443) and run the installer again."
)
