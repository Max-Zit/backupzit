package hyperv

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
	"github.com/max-zit/backupzit/internal/checker"
	"github.com/max-zit/backupzit/internal/repo"
)

// This opt-in integration test uses two independent processes and a running,
// OS-less, disconnected VM created specifically for this test. No existing VM
// may be selected. The setup/cleanup scripts stay outside the project patch.
func TestConcurrentBackupNative(t *testing.T) {
	root := os.Getenv("BACKUPZIT_TEST_HYPERV_ROOT")
	id := os.Getenv("BACKUPZIT_TEST_HYPERV_VM")
	if root == "" || !guidRe.MatchString(id) {
		t.Skip("dedicated BACKUPZIT_TEST_HYPERV_ROOT and VM GUID required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	vms, err := listVMs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, vm := range vms {
		if !strings.EqualFold(vm.ID, id) {
			continue
		}
		found = strings.HasPrefix(vm.Name, "BZF05-") && vm.State == "Running" && len(vm.Disks) == 1
		for _, disk := range vm.Disks {
			rel, err := filepath.Rel(filepath.Join(root, "work", "vm"), disk.Path)
			found = found && err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && !filepath.IsAbs(rel)
		}
	}
	if !found {
		t.Fatal("expected the dedicated running BZF05 VM with disks inside the test root")
	}
	run := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	start := func(role string) (*exec.Cmd, <-chan error) {
		cmd := exec.CommandContext(ctx, exe, "-test.run=^TestConcurrentBackupWorker$", "-test.v")
		cmd.Env = append(os.Environ(), "BACKUPZIT_TEST_HYPERV_WORKER="+role, "BACKUPZIT_TEST_HYPERV_RUN="+run)
		log, err := os.Create(filepath.Join(run, role+".log"))
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stdout, cmd.Stderr = log, log
		if err := cmd.Start(); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { err := cmd.Wait(); _ = log.Close(); done <- err }()
		return cmd, done
	}
	waitFile := func(name string) {
		for {
			if _, err := os.Stat(filepath.Join(run, name)); err == nil {
				return
			}
			select {
			case <-ctx.Done():
				t.Fatalf("waiting for %s: %v", name, ctx.Err())
			case <-time.After(100 * time.Millisecond):
			}
		}
	}
	_, firstDone := start("first")
	waitFile("first-checkpoint")
	// Always release the worker if an assertion fails; cleanup belongs to
	// the backup's own defer, not a broad checkpoint deletion in the test.
	defer os.WriteFile(filepath.Join(run, "release"), nil, 0o600)
	out, err := ps(ctx, `ConvertTo-Json -InputObject @(Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | Select-Object Id,Name) -Compress`)
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints []struct{ ID, Name string }
	if err := json.Unmarshal(out, &checkpoints); err != nil || len(checkpoints) != 1 {
		t.Fatalf("initial checkpoint: %s, %v", out, err)
	}
	checkpointID := checkpoints[0].ID
	_, secondDone := start("second")
	waitFile("second-started")
	preserved := true
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		out, err := ps(ctx, `[bool](Get-VMSnapshot -VM (Get-VM -Id `+psq(id)+`) | Where-Object { $_.Id -eq `+psq(checkpointID)+` }) | ConvertTo-Json -Compress`)
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(out)) != "true" {
			preserved = false
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(run, "second-finished")); err == nil {
		t.Error("second backup completed while the first still held its VM lock")
	}
	if err := os.WriteFile(filepath.Join(run, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, done := range []<-chan error{firstDone, secondDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("backup worker %d: %v", i+1, err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	for _, role := range []string{"first", "second"} {
		log, _ := os.ReadFile(filepath.Join(run, role+".log"))
		t.Logf("%s worker:\n%s", role, log)
	}
	if !preserved {
		t.Error("second backup removed the first backup's active checkpoint before its disk reads")
	}
	if err := os.WriteFile(filepath.Join(root, "validation", "native-"+os.Getenv("BACKUPZIT_TEST_HYPERV_LABEL")+".json"), []byte(fmt.Sprintf("{\"active_checkpoint_preserved\":%v,\"first_checkpoint\":%q}\n", preserved, checkpointID)), 0o600); err != nil {
		t.Error(err)
	}
}

func TestConcurrentBackupWorker(t *testing.T) {
	role := os.Getenv("BACKUPZIT_TEST_HYPERV_WORKER")
	if role == "" {
		t.Skip("subprocess worker only")
	}
	run := os.Getenv("BACKUPZIT_TEST_HYPERV_RUN")
	id := os.Getenv("BACKUPZIT_TEST_HYPERV_VM")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if role == "first" {
		original := ps
		defer func() { ps = original }()
		paused := false
		ps = func(ctx context.Context, script string) ([]byte, error) {
			out, err := original(ctx, script)
			var ci checkpointInfo
			if err == nil && !paused && strings.Contains(script, "CreateSnapshot") && json.Unmarshal(out, &ci) == nil && len(ci.Disks) > 0 {
				paused = true
				if err := os.WriteFile(filepath.Join(run, "first-checkpoint"), nil, 0o600); err != nil {
					return nil, err
				}
				for {
					if _, err := os.Stat(filepath.Join(run, "release")); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(50 * time.Millisecond):
					}
				}
			}
			return out, err
		}
	}
	be, err := backend.OpenLocal(filepath.Join(run, role+"-repo"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if err := os.WriteFile(filepath.Join(run, role+"-started"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	sn, err := Backup(ctx, r, BackupOptions{VMIDs: []int{VMIDFor(id)}, Version: "concurrency-regression"})
	if err != nil {
		t.Fatal(err)
	}
	if len(sn.Guests) != 1 || len(sn.Images) != 1 {
		t.Fatalf("incomplete backup: guests=%d errors=%v", len(sn.Guests), sn.Stats.Errors)
	}
	checked, err := checker.Run(ctx, r, checker.Options{ReadData: true})
	if err != nil || !checked.OK() {
		t.Fatalf("backup integrity: result=%+v error=%v", checked, err)
	}
	t.Logf("backup completed; consistency notes: %v", sn.Stats.Errors)
	if err := os.WriteFile(filepath.Join(run, role+"-finished"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
}
