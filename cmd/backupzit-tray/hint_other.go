//go:build !darwin

package main

// serviceHint tells what to do when the agent service does not answer.
const serviceHint = "Start the \"BackupZit Agent\" service or reinstall the agent."
