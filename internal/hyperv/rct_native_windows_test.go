package hyperv

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// Opt-in native reproduction: creates its own disconnected, OS-less VM. It
// never selects an existing VM and retains test files for inspection.
func TestRCTDiagnosticsNative(t *testing.T) {
	root := os.Getenv("BACKUPZIT_TEST_RCT_ROOT")
	if root == "" {
		t.Skip("BACKUPZIT_TEST_RCT_ROOT required for disposable Hyper-V fixture")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(root, "BZF08-")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	command := func(script string) []byte {
		t.Helper()
		out, err := ps(ctx, script)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	disk := filepath.Join(dir, "original.vhdx")
	command(`New-VHD -Path ` + psq(disk) + ` -SizeBytes 64MB -Dynamic | Out-Null`)
	fill := func(file string, value byte) {
		t.Helper()
		ad, err := attachVHD(file, false)
		if err != nil {
			t.Fatal(err)
		}
		defer ad.close()
		f, err := ad.openForWrite()
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteAt(bytes.Repeat([]byte{value}, diskimg.BlockSize), 0); err != nil {
			t.Fatal(err)
		}
		if err := f.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	fill(disk, 0x58)
	name := filepath.Base(dir)
	id := strings.TrimSpace(string(command(`$vm = New-VM -Name ` + psq(name) + ` -Generation 2 -MemoryStartupBytes 512MB -VHDPath ` + psq(disk) + ` -Path ` + psq(dir) + `; $vm.Id.Guid`)))
	if !guidRe.MatchString(id) {
		t.Fatalf("fixture GUID: %q", id)
	}
	defer func() {
		c, stop := context.WithTimeout(context.Background(), time.Minute)
		defer stop()
		_, err := ps(c, `$vm = Get-VM -Id `+psq(id)+`; if ($vm.Name -ne `+psq(name)+`) { throw 'fixture identity mismatch' }; Stop-VM -VM $vm -TurnOff -Force -ErrorAction SilentlyContinue; Remove-VM -VM $vm -Force`)
		if err != nil {
			t.Error(err)
		}
	}()
	command(`$vm = Get-VM -Id ` + psq(id) + `; Set-VM -VM $vm -AutomaticCheckpointsEnabled $false -CheckpointType Production; Set-VMFirmware -VM $vm -EnableSecureBoot Off; Get-VMNetworkAdapter -VM $vm | Disconnect-VMNetworkAdapter; Start-VM -VM $vm`)
	be, err := backend.OpenLocal(filepath.Join(dir, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var events []map[string]any
	backup := func() *repo.Snapshot {
		t.Helper()
		events = nil
		sn, err := Backup(ctx, r, BackupOptions{VMIDs: []int{VMIDFor(id)}, Version: "rct-diagnostics-regression", Log: func(msg string, kv ...any) {
			event := map[string]any{"message": msg}
			for i := 0; i+1 < len(kv); i += 2 {
				event[fmt.Sprint(kv[i])] = kv[i+1]
			}
			events = append(events, event)
		}})
		if err != nil || sn == nil || len(sn.Guests) != 1 || len(sn.Images) != 1 {
			t.Fatalf("native backup: %+v, %v", sn, err)
		}
		if sn.Guests[0].Disks[0].ChangeID == "" {
			t.Fatal("fixture did not obtain a real RCT ID; this host cannot validate the RCT query fallback")
		}
		return sn
	}
	first := backup()
	// Poison only the temporary repository's latest tracking position, not
	// VM state or the Virtual Disk API. The API must reject this real query.
	first.Time = first.Time.Add(time.Nanosecond)
	first.Guests[0].Disks[0].ChangeID = "deliberately-invalid-rct-id"
	if _, err := r.SaveSnapshot(ctx, first); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []string{"invalid-id", "resize", "replacement"} {
		size, value, reason := uint64(64<<20), byte(0x58), "rct_query_failed"
		if scenario != "invalid-id" {
			size = 96 << 20
			command(`Stop-VM -VM (Get-VM -Id ` + psq(id) + `) -TurnOff -Force`)
			if scenario == "resize" {
				command(`Resize-VHD -Path ` + psq(disk) + ` -SizeBytes 96MB`)
				reason = "disk_size_changed"
			} else {
				value = 0x38
				disk = filepath.Join(dir, "replacement.vhdx")
				command(`New-VHD -Path ` + psq(disk) + ` -SizeBytes 96MB -Dynamic | Out-Null`)
				fill(disk, value)
				command(`$vm = Get-VM -Id ` + psq(id) + `; Get-VMHardDiskDrive -VM $vm | Remove-VMHardDiskDrive; Add-VMHardDiskDrive -VM $vm -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0 -Path ` + psq(disk))
			}
			command(`Start-VM -VM (Get-VM -Id ` + psq(id) + `)`)
		}
		sn := backup()
		if !strings.Contains(sn.Images[0].Model, "(whole disk)") || sn.Stats.BytesRead != size {
			t.Fatalf("%s: expected a real whole-disk fallback: model=%s bytes_read=%d", scenario, sn.Images[0].Model, sn.Stats.BytesRead)
		}
		selection, completed := false, false
		for _, event := range events {
			t.Logf("%s event: %v", scenario, event)
			if event["message"] == "hyperv disk read" && event["reason"] == reason && event["mode"] == "full" && event["vm_id"] == id && event["disk"] == "scsi0:0" {
				selection = true
				if reason == "rct_query_failed" && event["error"] == nil {
					t.Error("missing underlying RCT error")
				}
			}
			if event["message"] == "hyperv disk read complete" && event["bytes_read"] == size {
				completed = true
			}
		}
		f, err := os.Create(filepath.Join(dir, scenario+".raw"))
		if err != nil {
			t.Fatal(err)
		}
		if err := diskimg.Write(ctx, r, &sn.Images[0], f, true, nil); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		buf, zero := make([]byte, diskimg.BlockSize), make([]byte, diskimg.BlockSize)
		for off := uint64(0); off < size; off += diskimg.BlockSize {
			want := zero
			if off == 0 {
				want = bytes.Repeat([]byte{value}, diskimg.BlockSize)
			}
			if _, err := f.ReadAt(buf, int64(off)); err != nil || !bytes.Equal(buf, want) {
				_ = f.Close()
				t.Fatalf("%s reconstructed data mismatch at %d: %v", scenario, off, err)
			}
		}
		_ = f.Close()
		t.Logf("%s: whole-disk fallback and all %d reconstructed bytes verified; reason_logged=%v bytes_logged=%v", scenario, size, selection, completed)
		if !selection || !completed {
			t.Errorf("%s: fallback reason/error and actual read volume absent from the backup log", scenario)
		}
	}
	checked, err := checker.Run(ctx, r, checker.Options{ReadData: true})
	if err != nil || !checked.OK() {
		t.Fatalf("repository integrity: %+v, %v", checked, err)
	}
}
