package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestPromptStopsAtEndOfInput(t *testing.T) {
	in := bufio.NewReader(strings.NewReader("https://srv:8443\n"))
	if s, err := prompt(in, "Server: "); err != nil || s != "https://srv:8443" {
		t.Fatalf("first answer: %q, %v", s, err)
	}
	// No more input (stdin closed): an error instead of an endless loop
	// of empty answers.
	if _, err := prompt(in, "Server: "); err == nil {
		t.Fatal("no error at the end of input")
	}
	// A last line without a newline still counts.
	in = bufio.NewReader(strings.NewReader("y"))
	if s, err := prompt(in, "OK? "); err != nil || s != "y" {
		t.Fatalf("last line: %q, %v", s, err)
	}
}
