package server

import (
	"strings"
	"testing"
)

func TestCheckSourcePatterns(t *testing.T) {
	newAgent, oldAgent := Agent{Hostname: "pc1", Version: "0.45.0"}, Agent{Hostname: "pc2", Version: "0.44.3"}
	files := func(p ...string) *Job { return &Job{Kind: JobFiles, Paths: p} }
	for _, c := range []struct {
		agent Agent
		job   *Job
		err   string
	}{
		{newAgent, files("{users}/Documents", `D:\Projects\*\Docs`), ""},
		{oldAgent, files(`D:\Data`, `D:\Projects [2024]`), ""}, // no patterns
		{oldAgent, files("{users}/Desktop"), "update it to 0.45.0"},
		{newAgent, &Job{Kind: JobNAS, Paths: []string{"team/*"}}, "not for NAS shares"},
		{oldAgent, &Job{Kind: JobVM, Paths: []string{"*"}}, ""}, // all VMs, not a path
	} {
		err := checkSourcePatterns(c.agent, c.job)
		if c.err == "" && err != nil || c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s %v: err = %v, want %q", c.agent.Version, c.job.Paths, err, c.err)
		}
	}
}
