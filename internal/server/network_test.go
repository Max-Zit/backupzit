package server_test

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/netcfg"
	"github.com/max-zit/backupzit/internal/server"
	"github.com/max-zit/backupzit/internal/update"
)

func TestNetworkSettings(t *testing.T) {
	e := setup(t)
	root := t.TempDir()
	sys := &netcfg.System{Root: root, Run: func(_ context.Context, name string, args ...string) (string, error) {
		if name == "ip" && len(args) > 2 && args[2] == "addr" {
			return `[{"ifname":"ens18","addr_info":[{"local":"192.168.1.50","prefixlen":24}]}]`, nil
		}
		if name == "ip" {
			return `[{"gateway":"192.168.1.1"}]`, nil
		}
		return "", nil
	}}
	defer server.UseNetSystem(sys)()
	admin := newClient(t, e)
	admin.login("admin", "admin-pass-123")

	// Not the appliance: explained, nothing to change.
	if _, _, body := admin.do("GET", "/settings/network", nil); !strings.Contains(body, "only on the BackupZit appliance") {
		t.Fatal("non-appliance not explained")
	}
	os.MkdirAll(filepath.Join(root, filepath.Dir(netcfg.ApplianceMarker)), 0o755)
	os.WriteFile(filepath.Join(root, netcfg.ApplianceMarker), nil, 0o644)
	os.WriteFile(filepath.Join(root, "etc", "hostname"), []byte("backupzit\n"), 0o644)
	data := t.TempDir()
	e.srv.StartUpdates(e.ctx, data)
	e.srv.EnableUpdateHelper()
	_, _, body := admin.do("GET", "/settings/network", nil)
	if !strings.Contains(body, "192.168.1.50/24") || !strings.Contains(body, "Apply network settings") || !strings.Contains(body, "Save SSH settings") {
		t.Fatal("network page incomplete")
	}
	// Invalid input is refused by the console, valid input goes to the helper.
	if _, loc, _ := admin.do("POST", "/settings/network", url.Values{"mode": {"static"}, "address": {"192.168.1.20"}, "dns": {"1.1.1.1"}}); !strings.Contains(loc, "err=") {
		t.Errorf("address without prefix accepted: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/settings/network", url.Values{"mode": {"static"}, "address": {"192.168.1.20/24"}, "gateway": {"192.168.1.1"}, "dns": {"192.168.1.1, 1.1.1.1"}, "hostname": {"backup01"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("network change: %s", loc)
	}
	var req update.Request
	update.ReadJSON(filepath.Join(data, "update", "request.json"), &req)
	if req.Action != update.ActionNetwork || req.Network == nil || req.Network.Address != "192.168.1.20/24" || len(req.Network.DNS) != 2 || req.Network.Hostname != "backup01" {
		t.Errorf("network request %+v", req)
	}
	if _, loc, _ := admin.do("POST", "/settings/ssh", url.Values{"enabled": {"on"}, "port": {"2222"}}); !strings.Contains(loc, "in+progress") {
		t.Errorf("second task: %s", loc)
	}
	os.Remove(filepath.Join(data, "update", "request.json"))
	if _, loc, _ := admin.do("POST", "/settings/ssh", url.Values{"enabled": {"on"}, "port": {"8443"}}); !strings.Contains(loc, "err=") {
		t.Errorf("console port accepted for SSH: %s", loc)
	}
	if _, loc, _ := admin.do("POST", "/settings/ssh", url.Values{"port": {"22"}}); !strings.Contains(loc, "msg=") {
		t.Fatalf("ssh off: %s", loc)
	}
	update.ReadJSON(filepath.Join(data, "update", "request.json"), &req)
	if req.Action != update.ActionSSH || req.SSH == nil || req.SSH.Enabled {
		t.Errorf("ssh request %+v", req)
	}
	if _, _, body := admin.do("GET", "/audit", nil); !strings.Contains(body, "settings.ssh") || !strings.Contains(body, "settings.network") {
		t.Error("changes not audited")
	}
}
