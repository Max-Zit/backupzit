package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/logfile"
)

// Limits of a log sent to the console (the request holds at most 4 MB).
const (
	maxLogSend    = 2 << 20
	maxUpdateLogs = 3
	maxUpdateLog  = 128 << 10
)

// sendLog sends the end of the agent's log and the newest installer logs of
// updates to the console, for troubleshooting without access to the
// machine.
func (a *Agent) sendLog(ctx context.Context, run api.Run) api.RunResult {
	var b strings.Builder
	host, osName, arch := hostInfo(a.version)
	fmt.Fprintf(&b, "BackupZit agent %s on %s (%s/%s), %s\nConsole: %s\nLog file: %s\n\n", a.version, host, osName, arch,
		time.Now().Format(time.RFC3339), a.client.cfg.ServerURL, a.LogPath)
	if a.LogPath == "" {
		b.WriteString("(this agent writes no log file)\n")
	} else if text, err := logfile.Tail(a.LogPath, logfile.DefaultKeep, maxLogSend); err != nil {
		fmt.Fprintf(&b, "(log file: %v)\n", err)
	} else {
		b.Write(text)
	}
	for _, p := range newestFiles(updateDir(), "*.log", maxUpdateLogs) {
		text, err := logfile.Tail(p, 0, maxUpdateLog)
		if err != nil {
			continue
		}
		fmt.Fprintf(&b, "\n===== %s =====\n", p)
		if runtime.GOOS == "windows" {
			text = utf16ToUTF8(text) // msiexec writes UTF-16 logs
		}
		b.Write(text)
	}
	err := a.client.post(ctx, fmt.Sprintf("%s%d/log", api.PathRunsPrefix, run.ID), api.AgentLog{Text: b.String()}, nil, true)
	if err != nil {
		return api.RunResult{Status: api.StatusFailed, Message: "send the log: " + err.Error()}
	}
	return api.RunResult{Status: api.StatusSuccess, Message: fmt.Sprintf("Log sent (%d KB)", (b.Len()+1023)/1024)}
}

// newestFiles lists up to n files of dir matching pattern, newest first.
func newestFiles(dir, pattern string, n int) []string {
	paths, _ := filepath.Glob(filepath.Join(dir, pattern))
	type fi struct {
		p string
		t time.Time
	}
	var list []fi
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
			list = append(list, fi{p, st.ModTime()})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].t.After(list[j].t) })
	var out []string
	for i := 0; i < len(list) && i < n; i++ {
		out = append(out, list[i].p)
	}
	return out
}

// utf16ToUTF8 converts a little-endian UTF-16 text (with or without BOM);
// other text is returned as it is.
func utf16ToUTF8(b []byte) []byte {
	start := 0
	switch {
	case len(b) >= 2 && b[0] == 0xFF && b[1] == 0xFE:
		start = 2
	case len(b) >= 4 && b[1] == 0 && b[3] == 0 && b[0] != 0:
	default:
		return b
	}
	var sb strings.Builder
	for i := start; i+1 < len(b); i += 2 {
		r := rune(b[i]) | rune(b[i+1])<<8
		if r != 0 && r != '\r' {
			sb.WriteRune(r)
		}
	}
	return []byte(sb.String())
}
