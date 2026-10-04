package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/api"
)

// backupKind reports whether runs of kind are backups (commands before and
// after them are allowed).
func backupKind(kind string) bool {
	switch kind {
	case api.KindBackup, api.KindImageBackup, api.KindVMBackup, api.KindSystemBackup, api.KindSQLBackup:
		return true
	}
	return false
}

// jobCommand runs a command configured for the job before ("pre") or after
// ("post") the backup. It is written into a script (cmd on Windows, sh
// elsewhere) so that several lines and any quoting work. It returns the
// combined output.
func (a *Agent) jobCommand(ctx context.Context, run api.Run, stage, command, status string) ([]byte, error) {
	timeout := time.Duration(run.CommandSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ext, shell, args := ".sh", "/bin/sh", []string{}
	if runtime.GOOS == "windows" {
		ext, shell, args = ".cmd", "cmd.exe", []string{"/D", "/C"}
		command = "@echo off\r\n" + strings.ReplaceAll(strings.ReplaceAll(command, "\r\n", "\n"), "\n", "\r\n") + "\r\n"
	}
	f, err := os.CreateTemp("", "backupzit-"+stage+"-*"+ext)
	if err != nil {
		return nil, err
	}
	defer os.Remove(f.Name())
	f.WriteString(command)
	f.Close()
	cmd := exec.CommandContext(ctx, shell, append(args, f.Name())...)
	cmd.Dir = filepath.Dir(f.Name())
	cmd.Env = append(os.Environ(), "BACKUPZIT_STAGE="+stage, "BACKUPZIT_JOB="+run.JobName,
		"BACKUPZIT_JOB_ID="+strconv.FormatInt(run.JobID, 10), "BACKUPZIT_RUN_ID="+strconv.FormatInt(run.ID, 10), "BACKUPZIT_STATUS="+status)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	killTree(cmd)
	cmd.WaitDelay = 5 * time.Second
	a.log.Info("job command", "run", run.ID, "stage", stage)
	err = cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out.Bytes(), fmt.Errorf("stopped after %s", timeout)
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return out.Bytes(), fmt.Errorf("exit code %d", ee.ExitCode())
		}
		return out.Bytes(), err
	}
	return out.Bytes(), nil
}

// commandOutput is the end of a command's output as run errors.
func commandOutput(out []byte) []string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return nil
	}
	if len(s) > 2000 {
		s = "…" + s[len(s)-2000:]
	}
	return []string{"command output: " + s}
}
