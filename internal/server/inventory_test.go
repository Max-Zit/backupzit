package server_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
)

// Refresh on the Agents page asks the agent, with its next poll only, for
// its inventory; agents before 0.54.1 get a clear message instead.
func TestAgentInventoryRefresh(t *testing.T) {
	e := setup(t)
	ctx := e.ctx
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")
	token, _, _ := e.store.CreateEnrollmentToken(ctx, time.Hour)
	cfg, err := agent.Enroll(ctx, e.ts.URL, token, e.fp, "0.54.0")
	if err != nil {
		t.Fatal(err)
	}
	agents, _ := e.store.ListAgents(ctx)
	id := agents[0].ID
	poll := func(version string) bool {
		b, _ := json.Marshal(api.PollRequest{Hostname: "pve", OS: "Linux", Arch: "amd64", Version: version})
		req, _ := http.NewRequest("POST", e.ts.URL+api.PathPoll, bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+cfg.AgentUUID+":"+cfg.Secret)
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var pr api.PollResponse
		json.NewDecoder(resp.Body).Decode(&pr)
		return pr.SendInventory
	}
	if _, _, body := admin.do("GET", "/agents", nil); !strings.Contains(body, fmt.Sprintf(`action="/agents/%d/inventory"`, id)) {
		t.Error("agents page lacks Refresh")
	}
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/inventory", id), nil); !strings.Contains(loc, "err=") || !strings.Contains(loc, "0.54.1") {
		t.Errorf("old agent: %s", loc)
	}
	poll("0.54.1") // the agent reports its new version
	if _, loc, _ := admin.do("POST", fmt.Sprintf("/agents/%d/inventory", id), nil); !strings.Contains(loc, "msg=") {
		t.Fatalf("refresh: %s", loc)
	}
	if !poll("0.54.1") {
		t.Error("the next poll does not ask for the inventory")
	}
	if poll("0.54.1") {
		t.Error("the inventory is asked for again")
	}
}
