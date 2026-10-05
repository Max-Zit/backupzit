package update

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/max-zit/backupzit/internal/netcfg"
)

// Network and SSH settings of the appliance, changed from the console
// (Settings → Network) through the root helper.
const (
	ActionNetwork = "network" // Request.Network
	ActionSSH     = "ssh"     // Request.SSH
)

// netSystem is the netcfg system behind a.Root, with a.Run.
func (a *Applier) netSystem() *netcfg.System {
	return &netcfg.System{Root: a.Root, Run: func(ctx context.Context, name string, args ...string) (string, error) {
		out, err := a.Run(ctx, name, args...)
		return string(out), err
	}}
}

// netAction applies a network or SSH request and reports in
// net-result.json (read by the console's network page).
func (a *Applier) netAction(ctx context.Context, req Request) (*Result, error) {
	res := OSResult{Action: req.Action, Status: "done"}
	sys := a.netSystem()
	var err error
	switch {
	case !sys.IsAppliance():
		err = errors.New("network settings can only be changed on the BackupZit appliance")
	case req.Action == ActionNetwork && req.Network != nil:
		// From the web console a static address whose gateway does not
		// answer is undone, so the appliance stays reachable.
		err = sys.Apply(ctx, *req.Network, true)
		res.Message = "network settings applied"
	case req.Action == ActionSSH && req.SSH != nil:
		err = sys.ApplySSH(ctx, *req.SSH)
		res.Message = "SSH settings applied"
	default:
		err = errors.New("incomplete network request")
	}
	if err != nil {
		res.Status, res.Message = "failed", err.Error()
	}
	res.Finished = time.Now().UTC()
	a.logf("%s: %s %s", req.Action, res.Status, res.Message)
	return nil, WriteJSON(filepath.Join(a.Dir, "net-result.json"), res, 0o644)
}
