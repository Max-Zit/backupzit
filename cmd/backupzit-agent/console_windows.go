package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetConsoleProcessList = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleProcessList")

// pauseIfStandalone keeps the window open when the program was started by
// double-clicking (its console belongs to this process alone), so the user
// can read the message instead of seeing a window flash and close.
func pauseIfStandalone() {
	var ids [2]uint32
	n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&ids[0])), 2)
	if n != 1 {
		return
	}
	fmt.Fprintln(os.Stderr, "\nThis is the BackupZit command line agent, not an installer.")
	fmt.Fprintln(os.Stderr, "To protect this computer, download and run the MSI installer from the")
	fmt.Fprintln(os.Stderr, "Agents page of your BackupZit console.")
	fmt.Fprint(os.Stderr, "\nPress Enter to close this window.")
	fmt.Scanln()
}
