package agent

import (
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

// The tray gets the console's web address from the console (0.61); an older
// console sends none and the address stays as it was (#62).
func TestRecordPollConsoleURL(t *testing.T) {
	a := &Agent{}
	a.recordPoll(nil, &api.AgentStatus{ConsoleURL: "https://backup.example.com"})
	a.recordPoll(nil, &api.AgentStatus{})
	if got := a.local().st.ConsoleURL; got != "https://backup.example.com" {
		t.Errorf("console address %q", got)
	}
}
