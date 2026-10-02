package main

import (
	"context"
	"fmt"
	"time"

	"github.com/backupzit/backupzit/internal/localipc"
)

// cmdStatus prints what the running agent service is doing.
func cmdStatus(ctx context.Context) error {
	resp, err := localipc.Call(ctx, localipc.Request{Cmd: localipc.CmdStatus})
	if err != nil {
		return fmt.Errorf("agent service not reachable (is it running and enrolled? run as administrator/root): %w", err)
	}
	st := resp.Status
	fmt.Printf("BackupZit agent %s on %s\n", st.Version, st.Hostname)
	conn := "not connected"
	if st.Connected {
		conn = "connected"
	}
	fmt.Printf("console:  %s (%s)\n", st.ServerURL, conn)
	if st.LastError != "" {
		fmt.Printf("          last error: %s\n", st.LastError)
	}
	ts := func(t *time.Time) string {
		if t == nil {
			return "â€”"
		}
		return t.Local().Format("2006-01-02 15:04")
	}
	if c := st.Current; c != nil {
		fmt.Printf("running:  %s %s (run %d) since %s", c.Kind, c.Job, c.ID, c.Started.Local().Format("15:04:05"))
		if c.Total > 0 {
			fmt.Printf(", %d%% of %s", c.Done*100/c.Total, humanBytes(c.Total))
		} else if c.Done > 0 {
			fmt.Printf(", %s read", humanBytes(c.Done))
		}
		fmt.Println()
	}
	if l := st.Last; l != nil {
		fmt.Printf("last run: %s %s â€” %s at %s: %s\n", l.Kind, l.Job, l.Status, ts(l.Finished), l.Message)
	}
	if st.JobsAt == nil {
		fmt.Println("jobs:     not received yet â€” run the command again in a few seconds")
		return nil
	}
	for _, j := range st.Jobs {
		fmt.Printf("job %-4d %-28s last %-8s %s   next %s\n", j.ID, j.Name, j.LastStatus, ts(j.LastFinished), ts(j.NextRun))
	}
	return nil
}
