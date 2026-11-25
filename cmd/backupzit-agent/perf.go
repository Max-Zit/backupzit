package main

import (
	"fmt"
	"os"
	"runtime/pprof"
	"sync"
	"time"

	"github.com/max-zit/backupzit/internal/backend"
)

// Throughput measurements of the command line (not used by the service):
// BACKUPZIT_PERF=1 counts the backend calls of the command and prints
// their time and bytes at the end; BACKUPZIT_CPUPROFILE=<file> writes a
// CPU profile of the command for go tool pprof.
var perf struct {
	mu    sync.Mutex
	on    bool
	start time.Time
	timed []*backend.Timed
	prof  *os.File
}

func perfStart() {
	perf.on = os.Getenv("BACKUPZIT_PERF") != ""
	perf.start = time.Now()
	if p := os.Getenv("BACKUPZIT_CPUPROFILE"); p != "" {
		f, err := os.Create(p)
		if err == nil && pprof.StartCPUProfile(f) == nil {
			perf.prof = f
		} else {
			fmt.Fprintln(os.Stderr, "perf: no CPU profile:", err)
		}
	}
}

// perfWrap counts the calls of be when BACKUPZIT_PERF is set. Storage that
// makes objects immutable keeps its own type (it is not measured).
func perfWrap(be backend.Backend) backend.Backend {
	if !perf.on {
		return be
	}
	if _, ok := be.(backend.Retainer); ok {
		return be
	}
	t := backend.NewTimed(be)
	perf.mu.Lock()
	perf.timed = append(perf.timed, t)
	perf.mu.Unlock()
	return t
}

func perfStop() {
	if perf.prof != nil {
		pprof.StopCPUProfile()
		_ = perf.prof.Close()
	}
	if !perf.on {
		return
	}
	wall := time.Since(perf.start)
	perf.mu.Lock()
	defer perf.mu.Unlock()
	if len(perf.timed) == 0 {
		fmt.Fprintf(os.Stderr, "perf: wall %.1fs\n", wall.Seconds())
	}
	for _, t := range perf.timed {
		fmt.Fprintln(os.Stderr, t.Report(wall))
	}
}
