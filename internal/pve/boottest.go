package pve

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// BootResult is the outcome of booting one VM of a backup.
type BootResult struct {
	VMID    int    `json:"vmid"` // guest in the backup
	Name    string `json:"name"`
	Booted  bool   `json:"booted"`  // the QEMU guest agent answered
	Running bool   `json:"running"` // the VM kept running (no guest agent to ask)
	Seconds int    `json:"seconds"`
	Error   string `json:"error,omitempty"`
}

// BootTest starts a VM of a backup as an isolated instant VM (network cards
// disconnected), waits until its QEMU guest agent answers, and deletes it
// again. VMs without the guest agent count as running when they did not
// stop within a minute.
func BootTest(ctx context.Context, sn *repo.Snapshot, vmid int, timeout time.Duration, opts InstantOptions) BootResult {
	res := BootResult{VMID: vmid}
	g, err := FindGuest(sn, vmid)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	res.Name = g.Name
	opts.VMID, opts.NewVMID, opts.Start, opts.Isolated = vmid, -1, true, true
	if opts.Name == "" {
		opts.Name = "boot-test-" + strconv.Itoa(vmid)
	}
	start := time.Now()
	ir, err := InstantStart(ctx, sn, opts)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer InstantDiscard(context.Background(), ir.VMID)
	if !ir.Started {
		res.Error = "the VM did not start: " + strings.Join(ir.Notes, "; ")
		return res
	}
	id := strconv.Itoa(ir.VMID)
	agent := hasGuestAgent(g.Config)
	deadline := start.Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			res.Error = ctx.Err().Error()
			return res
		case <-time.After(5 * time.Second):
		}
		out, _ := command(ctx, "qm", "status", id)
		if !strings.Contains(string(out), "running") {
			res.Error = "the VM stopped by itself (crashed or shut down) " + time.Since(start).Round(time.Second).String() + " after starting"
			return res
		}
		if agent {
			if _, err := command(ctx, "qm", "agent", id, "ping"); err == nil {
				res.Booted, res.Seconds = true, int(time.Since(start).Seconds())
				return res
			}
		} else if time.Since(start) >= time.Minute {
			res.Running, res.Seconds = true, int(time.Since(start).Seconds())
			return res
		}
	}
	res.Error = fmt.Sprintf("the QEMU guest agent did not answer within %s; the operating system may not have booted", timeout)
	return res
}

var agentOn = regexp.MustCompile(`(?m)^agent:\s*(1|enabled=1|.*,enabled=1)`)

func hasGuestAgent(conf string) bool {
	if i := strings.Index(conf, "\n["); i >= 0 {
		conf = conf[:i]
	}
	return agentOn.MatchString(conf)
}

// isolateConfig disconnects the network cards and drops autostart, for
// VMs that must not reach the network (boot tests).
func isolateConfig(conf string) string {
	var b strings.Builder
	for _, l := range strings.SplitAfter(conf, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\n"), ":")
		switch {
		case ok && k == "onboot":
			continue
		case ok && qemuNetRe.MatchString(k):
			v = strings.TrimSpace(v)
			if !strings.Contains(","+v+",", ",link_down=1,") {
				v += ",link_down=1"
			}
			l = k + ": " + v + "\n"
		}
		b.WriteString(l)
	}
	return b.String()
}
