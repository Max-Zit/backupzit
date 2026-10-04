package agent

import (
	"context"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/backupzit/backupzit/internal/api"
)

func TestJobCommand(t *testing.T) {
	a := &Agent{log: slog.Default()}
	run := api.Run{ID: 7, JobName: "docs", CommandSeconds: 30}
	echo, fail, slow := "echo stage=$BACKUPZIT_STAGE status=$BACKUPZIT_STATUS\necho 'quoted \"text\"'", "echo broken >&2\nexit 3", "sleep 5"
	if runtime.GOOS == "windows" {
		echo, fail, slow = "echo stage=%BACKUPZIT_STAGE% status=%BACKUPZIT_STATUS%\necho 'quoted \"text\"'", "echo broken 1>&2\nexit /b 3", "ping -n 6 127.0.0.1 >nul"
	}
	out, err := a.jobCommand(context.Background(), run, "post", echo, "warning")
	if err != nil || !strings.Contains(string(out), "stage=post status=warning") || !strings.Contains(string(out), `quoted "text"`) {
		t.Fatalf("output %q, %v", out, err)
	}
	out, err = a.jobCommand(context.Background(), run, "pre", fail, "")
	if err == nil || err.Error() != "exit code 3" || !strings.Contains(string(out), "broken") {
		t.Fatalf("failing command: %q, %v", out, err)
	}
	if e := commandOutput(out); len(e) != 1 || !strings.Contains(e[0], "broken") {
		t.Errorf("commandOutput = %q", e)
	}
	run.CommandSeconds = 1
	start := time.Now()
	if _, err := a.jobCommand(context.Background(), run, "pre", slow, ""); err == nil || !strings.Contains(err.Error(), "stopped after") || time.Since(start) > 4*time.Second {
		t.Fatalf("slow command: %v after %s", err, time.Since(start))
	}
}
