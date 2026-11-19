package vss

import (
	"errors"
	"strings"
	"testing"

	ole "github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
)

func TestResolveSnapshotProperties(t *testing.T) {
	id := ole.GUID{Data1: 1}
	dev := func(s string) *uint16 {
		p, err := windows.UTF16PtrFromString(s)
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	const shadow = `\\?\GLOBALROOT\Device\HarddiskVolumeShadowCopy7`
	withDevice := func(_ ole.GUID, p *vssSnapshotProperties) error {
		p.snapshotID, p.snapshotDeviceObject = id, dev(shadow)
		return nil
	}
	// The QEMU guest agent's provider: S_OK, nothing filled in.
	empty := func(ole.GUID, *vssSnapshotProperties) error { return nil }
	failing := func(ole.GUID, *vssSnapshotProperties) error { return errors.New("query broken") }
	mustNotRun := func(ole.GUID, *vssSnapshotProperties) error {
		t.Error("Query called although GetSnapshotProperties returned a device")
		return nil
	}

	for _, tc := range []struct {
		name       string
		get, query func(ole.GUID, *vssSnapshotProperties) error
		want       string // device, or "" for an error
		wantErr    string
		wantFrees  int
	}{
		{name: "device from GetSnapshotProperties", get: withDevice, query: mustNotRun, want: shadow},
		{name: "empty, device from Query", get: empty, query: withDevice, want: shadow, wantFrees: 1},
		{name: "empty everywhere", get: empty, query: empty, wantErr: "no device", wantFrees: 2},
		{name: "empty, Query fails", get: empty, query: failing, wantErr: "query broken", wantFrees: 1},
		{name: "GetSnapshotProperties fails", get: failing, query: mustNotRun, wantErr: "query broken"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frees := 0
			p, err := resolveSnapshotProperties(id, tc.get, tc.query, func(*vssSnapshotProperties) { frees++ })
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want it to contain %q", err, tc.wantErr)
				}
				if p.GetSnapshotDeviceObject() != "" {
					t.Errorf("device %q returned with an error", p.GetSnapshotDeviceObject())
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if got := p.GetSnapshotDeviceObject(); got != tc.want {
					t.Errorf("device = %q, want %q", got, tc.want)
				}
			}
			if frees != tc.wantFrees {
				t.Errorf("freed %d times, want %d", frees, tc.wantFrees)
			}
		})
	}
}
