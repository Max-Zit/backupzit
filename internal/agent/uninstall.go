package agent

import (
	"github.com/max-zit/backupzit/internal/api"
)

// uninstall removes the agent from this machine when it is removed in the
// console. The result is reported first; the uninstaller runs afterwards
// as an independent process, because it stops this service.
func (a *Agent) uninstall() (api.RunResult, func()) {
	start, err := uninstallerCommand()
	if err != nil {
		return failed(err), nil
	}
	a.log.Info("removed in the console: uninstalling the agent")
	return api.RunResult{Status: api.StatusSuccess, Message: "The agent is uninstalling itself from this computer."}, func() {
		if err := start(); err != nil {
			a.log.Error("uninstall", "err", err)
		}
	}
}
