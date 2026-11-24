package main

import (
	"fmt"
	"testing"
)

// ntfs-check --records takes record 0 ($MFT) too, and rejects what is not
// a record number.
func TestParseRecordList(t *testing.T) {
	got, err := parseRecordList(" 0, 22335 ,45256")
	if err != nil || fmt.Sprint(got) != "[0 22335 45256]" {
		t.Errorf("%v %v", got, err)
	}
	if got, err := parseRecordList(""); err != nil || got != nil {
		t.Errorf("empty: %v %v", got, err)
	}
	for _, bad := range []string{"-1", "x", "1,,2", "281474976710656"} {
		if _, err := parseRecordList(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
