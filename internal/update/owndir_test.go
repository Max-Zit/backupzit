package update

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// L-N23: the console's user may replace the update folder with a link
// (to /etc); the root helper then neither reads the request there nor
// writes its results or removes anything in the folder the link points
// to.
func TestApplyRefusesLinkedUpdateFolder(t *testing.T) {
	var up atomic.Bool
	r := newRelease(t, "0.28.0")
	a, calls := setupApply(t, r, &up)
	elsewhere := a.Dir + ".real"
	if err := os.Rename(a.Dir, elsewhere); err != nil {
		t.Fatal(err)
	}
	linkDir(t, elsewhere, a.Dir)
	if res, err := a.Apply(context.Background()); err == nil || res != nil {
		t.Fatalf("a linked update folder was used: %+v %v", res, err)
	}
	if len(*calls) != 0 {
		t.Errorf("commands ran: %v", *calls)
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "request.json")); err != nil {
		t.Error("the request in the linked folder was removed")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, r.name)); err != nil {
		t.Error("the package in the linked folder was removed")
	}
	if _, err := os.Stat(filepath.Join(elsewhere, "result.json")); err == nil {
		t.Error("a result was written into the linked folder")
	}
}

// request.json itself a link to a file outside the folder: not read; only
// the link goes, and the request fails.
func TestApplyRefusesLinkedRequest(t *testing.T) {
	var up atomic.Bool
	r := newRelease(t, "0.28.0")
	a, calls := setupApply(t, r, &up)
	outside := filepath.Join(t.TempDir(), "secret.json")
	if err := os.WriteFile(outside, []byte(`{"file":"`+r.name+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	req := filepath.Join(a.Dir, "request.json")
	if err := os.Remove(req); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, req); err != nil {
		t.Skip("symlinks not available:", err)
	}
	res, err := a.Apply(context.Background())
	if err != nil || res == nil || res.Status != "failed" || !strings.Contains(res.Message, "invalid update request") {
		t.Fatalf("request through a link: %+v %v", res, err)
	}
	if len(*calls) != 0 {
		t.Errorf("commands ran: %v", *calls)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("the file the link pointed to was removed")
	}
	var saved Result
	if ReadJSON(filepath.Join(a.Dir, "result.json"), &saved); saved.Status != "failed" {
		t.Errorf("result.json %+v", saved)
	}
}

func TestApplyRefusesWorldWritableFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes: Linux only (the helper runs there)")
	}
	var up atomic.Bool
	r := newRelease(t, "0.28.0")
	a, calls := setupApply(t, r, &up)
	if err := os.Chmod(a.Dir, 0o777); err != nil { // #nosec G302 -- the case under test
		t.Fatal(err)
	}
	if res, err := a.Apply(context.Background()); err == nil || res != nil || len(*calls) != 0 {
		t.Fatalf("a folder every user may write to was used: %+v %v %v", res, err, *calls)
	}
}

// linkDir makes link point to the folder target: a symbolic link, or on
// Windows without the right for one a junction (Lstat shows neither as a
// folder).
func linkDir(t *testing.T, target, link string) {
	t.Helper()
	err := os.Symlink(target, link)
	if err == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Clean(link), filepath.Clean(target)).CombinedOutput(); err != nil { // #nosec G204 -- test paths
		t.Skipf("no symbolic link or junction: %v %s", err, out)
	}
}
