package hyperv

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// MED 13a: the recovery script takes only a new checkpoint of type
// Recovery, and only when exactly one appeared; with more (another backup
// program at the same time) it renames and removes none.
func TestRecoveryScriptTakesOnlyItsOwnCheckpoint(t *testing.T) {
	for _, want := range []string{
		`$before -notcontains $_.Id -and "$($_.SnapshotType)" -eq 'Recovery'`,
		`if ($new.Count -gt 1) { throw ("` + ambiguousCheckpoint,
		`$s = $new[0]`,
	} {
		if !strings.Contains(recoveryScript, want) {
			t.Errorf("recovery script lacks %q", want)
		}
	}
	if strings.Contains(recoveryScript, "Select-Object -First 1") {
		t.Error("recovery script still takes the first new checkpoint")
	}
	// The ambiguity is thrown before anything is renamed or removed.
	amb := strings.Index(recoveryScript, ambiguousCheckpoint)
	if ren := strings.Index(recoveryScript, "Rename-VMSnapshot"); ren < amb {
		t.Error("the checkpoint is renamed before the ambiguity is checked")
	}
}

// When the script reports several new recovery checkpoints, the backup
// fails at once with an explanation: no crash-consistent retry, no
// production checkpoint, and no removal of a checkpoint by name.
func TestCreateCheckpointAmbiguous(t *testing.T) {
	const name = "backupzit_ambiguous"
	vm := vmInfo{ID: "5a86f77b-3849-4ca5-b1d6-292ce1c9d647", Name: "web01", State: "Running"}
	original := ps
	defer func() { ps = original }()
	var scripts []string
	ps = func(_ context.Context, script string) ([]byte, error) {
		scripts = append(scripts, script)
		if strings.Contains(script, ").Count") { // checkpointExists: ours was never named
			return []byte("0"), nil
		}
		if strings.Contains(script, "CreateSnapshot") {
			return nil, errors.New("exit status 1: " + ambiguousCheckpoint + ": 2")
		}
		t.Errorf("unexpected script: %.80s", script)
		return nil, errors.New("unexpected")
	}
	_, _, _, _, err := takeCheckpoint(context.Background(), vm, name, false, false)
	if err == nil || !strings.Contains(err.Error(), "cannot tell which one is its own") || !strings.Contains(err.Error(), "web01") {
		t.Fatalf("err = %v", err)
	}
	creates := 0
	for _, s := range scripts {
		if strings.Contains(s, "CreateSnapshot") || strings.Contains(s, "Checkpoint-VM") {
			creates++
		}
		if strings.Contains(s, "Remove-VMSnapshot") && !strings.Contains(s, psq(name)) {
			t.Errorf("a checkpoint other than the backup's own name was removed: %.120s", s)
		}
	}
	if creates != 1 {
		t.Errorf("%d checkpoint creations, want 1 (no retry, no production checkpoint)", creates)
	}
}
