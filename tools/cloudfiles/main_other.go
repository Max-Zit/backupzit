//go:build !windows

// Command cloudfiles is a Windows lab tool (cloud files placeholders); see
// main_windows.go.
package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "cloudfiles runs on Windows only")
	os.Exit(2)
}
