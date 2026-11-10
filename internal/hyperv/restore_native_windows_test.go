package hyperv

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/diskimg"
	"github.com/max-zit/backupzit/internal/repo"
)

// Opt-in: creates disposable, disconnected, OS-less VMs below the explicitly
// selected root. It never restores over a pre-existing VM. Requires admin,
// Hyper-V and enough memory to start a 512 MiB Generation 2 fixture.
func TestRestoreStagingNative(t *testing.T) {
	root := os.Getenv("BACKUPZIT_TEST_RESTORE_ROOT")
	if root == "" {
		t.Skip("BACKUPZIT_TEST_RESTORE_ROOT required for disposable Hyper-V fixtures")
	}
	if !filepath.IsAbs(root) {
		t.Fatal("fixture root must be absolute")
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, failure := range []string{"missing-image", "missing-data", "success", "cutover-error"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			dir, err := os.MkdirTemp(root, "BZF09-")
			if err != nil {
				t.Fatal(err)
			}
			name := filepath.Base(dir)
			base := filepath.Join(dir, "original.vhdx")
			out, err := ps(ctx, `New-VHD -Path `+psq(base)+` -SizeBytes 64MB -Dynamic | Out-Null; $vm = New-VM -Name `+psq(name)+` -Generation 2 -MemoryStartupBytes 512MB -VHDPath `+psq(base)+` -Path `+psq(dir)+`; $vm.Id.Guid`)
			if err != nil {
				t.Fatal(err)
			}
			id := strings.TrimSpace(string(out))
			if !guidRe.MatchString(id) {
				t.Fatalf("fixture GUID: %q", id)
			}
			// Deliberately retain fixture disks as local evidence. Only the VM
			// created above is unregistered, even if an assertion fails.
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Minute)
				defer stop()
				if _, err := ps(cleanup, `$vm = Get-VM -Id `+psq(id)+`; if ($vm.Name -ne `+psq(name)+`) { throw 'fixture identity mismatch' }; Stop-VM -VM $vm -TurnOff -Force -ErrorAction SilentlyContinue; Remove-VM -VM $vm -Force`); err != nil {
					t.Error(err)
				}
			}()
			if _, err := ps(ctx, `$vm = Get-VM -Id `+psq(id)+`; Set-VM -VM $vm -AutomaticCheckpointsEnabled $false -CheckpointType Standard; Set-VMFirmware -VM $vm -EnableSecureBoot Off; Get-VMNetworkAdapter -VM $vm | Disconnect-VMNetworkAdapter; Start-VM -VM $vm; Checkpoint-VM -VM $vm -SnapshotName 'F09-original'`); err != nil {
				t.Fatal(err)
			}
			before, err := getVM(ctx, id)
			if err != nil || before == nil || before.State != "Running" || len(before.Disks) != 1 {
				t.Fatalf("fixture before: %+v, %v", before, err)
			}
			checkpoint, err := ps(ctx, `(Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`)).Id.Guid`)
			if err != nil || !guidRe.MatchString(strings.TrimSpace(string(checkpoint))) {
				t.Fatalf("fixture checkpoint: %s, %v", checkpoint, err)
			}
			be, err := backend.OpenLocal(filepath.Join(dir, "repo"))
			if err != nil {
				t.Fatal(err)
			}
			r, err := repo.Init(ctx, be)
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			// One nonzero block and implicit zero blocks: exercise actual blob
			// reads and raw VHD writes without an OS or production backup.
			payload := bytes.Repeat([]byte{0x59}, diskimg.BlockSize)
			dataID, _, err := r.SaveBlob(ctx, repo.DataBlob, payload)
			if err != nil {
				t.Fatal(err)
			}
			ids := make([]repo.ID, 64)
			ids[0] = dataID
			if failure == "missing-data" {
				ids[0] = repo.Hash([]byte("deliberately absent F09 data blob"))
			}
			maps, err := r.SaveBlockMap(ctx, ids)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Flush(ctx); err != nil {
				t.Fatal(err)
			}
			cfg, _ := json.Marshal(before)
			sn := &repo.Snapshot{Time: time.Now(), Guests: []repo.Guest{{Platform: "hyperv", VMID: VMIDFor(id), ID: id, Config: string(cfg), Disks: []repo.GuestDisk{{Key: "scsi0:0", Size: 64 << 20, Image: 0}}}}, Images: []repo.DiskImage{{Size: 64 << 20, Style: "raw", Partitions: []repo.PartitionImage{{Length: 64 << 20, BlockSize: diskimg.BlockSize, Blocks: 64, Maps: maps, Included: true}}}}}
			if failure == "missing-image" {
				sn.Guests[0].Disks[0].Image = -1
			}
			if failure == "cutover-error" {
				originalPS := ps
				defer func() { ps = originalPS }()
				ps = func(ctx context.Context, script string) ([]byte, error) {
					out, err := originalPS(ctx, script)
					if err == nil && strings.Contains(script, "Add-VMHardDiskDrive -VM $vm") {
						return out, errors.New("injected lost response after actual disk attachment")
					}
					return out, err
				}
			}
			res, restoreErr := Restore(ctx, r, sn, RestoreOptions{VMID: VMIDFor(id), Overwrite: true, Log: func(msg string, args ...any) { t.Log(msg, args) }})
			after, err := getVM(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if failure == "cutover-error" {
				if restoreErr == nil || !strings.Contains(restoreErr.Error(), "restored disks retained in") || len(after.Disks) != 1 {
					t.Fatalf("expected recoverable cutover error: %v, %+v", restoreErr, after)
				}
				if _, err := os.Stat(after.Disks[0].Path); err != nil {
					t.Fatalf("attached reconstructed disk was deleted after cutover failure: %v", err)
				}
				if _, err := os.Stat(base); err != nil {
					t.Fatal(err)
				}
				t.Logf("cutover error retains attached restored disk and original: %v", restoreErr)
				return
			}
			if failure != "success" {
				if restoreErr == nil {
					t.Fatal("expected restore failure")
				}
				t.Logf("expected failure: %v; VM state before=%s after=%s, disks before=%v after=%v", restoreErr, before.State, after.State, before.Disks, after.Disks)
				if failure == "missing-data" && !strings.Contains(restoreErr.Error(), "block 0:") {
					t.Errorf("did not exercise missing data read: %v", restoreErr)
				}
				if after.State != before.State || len(after.Disks) != 1 || !strings.EqualFold(after.Disks[0].Path, before.Disks[0].Path) {
					t.Error("failed disk reconstruction changed the original VM's state or attachment")
				}
				out, err := ps(ctx, `[bool](Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | Where-Object Id -eq `+psq(strings.TrimSpace(string(checkpoint)))+`)`)
				if err != nil || strings.TrimSpace(string(out)) != "True" {
					t.Errorf("original checkpoint lost: %s, %v", out, err)
				}
				staged, _ := filepath.Glob(filepath.Join(dir, "backupzit-restore-*"))
				if len(staged) != 0 {
					t.Errorf("failed preparation left staging directories: %v", staged)
				}
				return
			}
			if restoreErr != nil {
				t.Fatal(restoreErr)
			}
			if res.ID != id || after.State != "Off" || len(after.Disks) != 1 || strings.EqualFold(after.Disks[0].Path, before.Disks[0].Path) {
				t.Fatalf("successful cutover: %+v, %+v", res, after)
			}
			if _, err := os.Stat(base); err != nil {
				t.Fatalf("original disk was moved or removed: %v", err)
			}
			if fmt.Sprint(before.NICs) != fmt.Sprint(after.NICs) {
				t.Errorf("network identity changed: before=%v after=%v", before.NICs, after.NICs)
			}
			ad, err := attachVHD(after.Disks[0].Path, true)
			if err != nil {
				t.Fatal(err)
			}
			defer ad.close()
			f, err := os.Open(ad.path)
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			got := make([]byte, len(payload))
			if _, err := f.ReadAt(got, 0); err != nil || !bytes.Equal(got, payload) {
				t.Fatalf("restored disk content mismatch: %v", err)
			}
			zeros := make([]byte, diskimg.BlockSize)
			for i := int64(1); i < 64; i++ {
				if _, err := f.ReadAt(got, i*diskimg.BlockSize); err != nil || !bytes.Equal(got, zeros) {
					t.Fatalf("restored zero block %d mismatch: %v", i, err)
				}
			}
			t.Logf("cutover succeeded: original=%s, restored=%s, all 64 MiB match repository data", base, after.Disks[0].Path)
		})
	}
}
