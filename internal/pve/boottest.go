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
	// Snapshot is the tested backup (0.61: a test can also boot the oldest
	// kept one); Checks are the job's checks inside the VM; Passed means
	// the guest agent answered and every check passed.
	Snapshot string        `json:"snapshot,omitempty"`
	Checks   []CheckResult `json:"checks,omitempty"`
	Passed   bool          `json:"passed,omitempty"`
	// LeftOut are the settings the isolated copy was started without
	// (hookscript, args, passed-through devices, …, 0.62).
	LeftOut []string `json:"left_out,omitempty"`
}

// BootTest starts a VM of a backup as an isolated instant VM (network cards
// disconnected), waits until its QEMU guest agent answers, and deletes it
// again. VMs without the guest agent count as running when they did not
// stop within a minute. checks run in the booted VM through the guest agent;
// each may take until timeout after the agent answered.
func BootTest(ctx context.Context, sn *repo.Snapshot, vmid int, timeout time.Duration, checks []Check, opts InstantOptions) BootResult {
	res := BootResult{VMID: vmid, Snapshot: sn.ID.String()}
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
	res.LeftOut = ir.LeftOut
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
				if len(checks) > 0 {
					res.Checks = runChecks(ctx, id, checks, time.Now().Add(timeout))
				}
				res.Passed = true
				for _, c := range res.Checks {
					if !c.OK {
						res.Passed = false
						res.Error = "check failed: " + c.Check + " (" + c.Detail + ")"
						break
					}
				}
				return res
			}
		} else if time.Since(start) >= time.Minute {
			res.Running, res.Seconds = true, int(time.Since(start).Seconds())
			if len(checks) > 0 {
				res.Error = "the checks need the QEMU guest agent in the VM (agent: 1 in its configuration and qemu-guest-agent installed)"
			}
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

// isolatedDropRe are the settings an isolated VM leaves out: a hook script
// runs on the host, raw QEMU arguments can add network devices, and passed
// through PCI or USB devices, host shares (virtiofs) and serial or
// parallel ports of the host reach outside the VM.
var isolatedDropRe = regexp.MustCompile(`^(hookscript|args|hostpci\d+|usb\d+|virtiofs\d+)$`)

var hostPortRe = regexp.MustCompile(`^(serial|parallel)\d+$`)

// isolateConfig disconnects the network cards and drops autostart and the
// settings that reach outside the VM (isolatedDropRe, serial and parallel
// ports on host devices), for VMs that must not reach the network (boot
// tests). It returns the keys it left out (each once).
func isolateConfig(conf string) (string, []string) {
	var b strings.Builder
	var dropped []string
	seen := map[string]bool{}
	for _, l := range strings.SplitAfter(conf, "\n") {
		k, v, ok := strings.Cut(strings.TrimRight(l, "\n"), ":")
		switch {
		case ok && k == "onboot":
			continue
		case ok && (isolatedDropRe.MatchString(k) || hostPortRe.MatchString(k) && strings.HasPrefix(strings.TrimSpace(v), "/dev/")):
			if !seen[k] {
				seen[k] = true
				dropped = append(dropped, k)
			}
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
	return b.String(), dropped
}
