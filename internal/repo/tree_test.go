package repo

import (
	"encoding/json"
	"testing"
	"time"
)

// The macOS fields of UnixMeta are optional: a node without them (Linux)
// encodes exactly as before they existed, so tree blobs of Linux agents
// keep the same bytes and deduplicate against older snapshots.
func TestUnixMetaJSONWithoutMacFields(t *testing.T) {
	m := UnixMeta{UID: 1000, GID: 1000, User: "bob", Group: "staff", LinkKey: "1:2"}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"uid":1000,"gid":1000,"user":"bob","group":"staff","link_key":"1:2"}`
	if string(b) != want {
		t.Errorf("encoding changed:\n got %s\nwant %s", b, want)
	}
}

// Flags and creation time survive an encode/decode round trip, and a tree
// written before they existed decodes with both zero.
func TestUnixMetaJSONMacFields(t *testing.T) {
	bt := time.Date(2001, 2, 3, 4, 5, 6, 789, time.UTC)
	m := UnixMeta{UID: 501, GID: 20, Flags: 0x8001, Birthtime: bt}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var got UnixMeta
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Flags != 0x8001 || !got.Birthtime.Equal(bt) {
		t.Errorf("round trip: flags %#x birthtime %v (json %s)", got.Flags, got.Birthtime, b)
	}
	var old UnixMeta
	if err := json.Unmarshal([]byte(`{"uid":0,"gid":0}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.Flags != 0 || !old.Birthtime.IsZero() {
		t.Errorf("old node: flags %#x birthtime %v", old.Flags, old.Birthtime)
	}
}
