package netcfg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidate(t *testing.T) {
	for _, c := range []struct {
		cfg Config
		ok  bool
	}{
		{Config{DHCP: true}, true},
		{Config{DHCP: true, Address: "x"}, true}, // ignored with DHCP
		{Config{Address: "192.168.1.20/24", Gateway: "192.168.1.1", DNS: []string{"192.168.1.1"}}, true},
		{Config{Address: "192.168.1.20", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}, false},
		{Config{Address: "192.168.1.0/24", DNS: []string{"1.1.1.1"}}, false},
		{Config{Address: "192.168.1.20/24", Gateway: "10.0.0.1", DNS: []string{"1.1.1.1"}}, false},
		{Config{Address: "192.168.1.20/24", Gateway: "192.168.1.1"}, false},
		{Config{Address: "192.168.1.20/24", DNS: []string{"dns.example"}}, false},
		{Config{DHCP: true, Hostname: "backup01"}, true},
		{Config{DHCP: true, Hostname: "backup.example.com"}, false},
		{Config{DHCP: true, Hostname: "bad name;rm"}, false},
	} {
		cfg := c.cfg
		if err := cfg.Validate(); (err == nil) != c.ok {
			t.Errorf("%+v: %v", c.cfg, err)
		}
	}
}

func TestNetworkFileRoundTrip(t *testing.T) {
	c := Config{Address: "10.0.0.5/24", Gateway: "10.0.0.1", DNS: []string{"10.0.0.1", "1.1.1.1"}}
	got := parseNetworkFile(networkFile(c))
	if got.DHCP || got.Address != c.Address || got.Gateway != c.Gateway || strings.Join(got.DNS, ",") != "10.0.0.1,1.1.1.1" {
		t.Errorf("%+v", got)
	}
	if !parseNetworkFile(networkFile(Config{DHCP: true})).DHCP {
		t.Error("dhcp lost")
	}
}

func fakeSystem(t *testing.T, gatewayAnswers bool) (*System, *[]string) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "etc/network"), 0o755)
	os.WriteFile(filepath.Join(root, "etc/network/interfaces"), []byte("auto lo\niface lo inet loopback\nallow-hotplug ens18\niface ens18 inet dhcp\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/hosts"), []byte("127.0.0.1\tlocalhost\n127.0.1.1\told\n"), 0o644)
	os.WriteFile(filepath.Join(root, "etc/resolv.conf"), []byte("nameserver 9.9.9.9\n"), 0o644)
	var calls []string
	s := &System{Root: root, GatewayWait: 3 * time.Second, Run: func(_ context.Context, name string, args ...string) (string, error) {
		calls = append(calls, name+" "+strings.Join(args, " "))
		switch {
		case name == "ping" && !gatewayAnswers:
			return "", errors.New("no answer")
		case name == "ip" && len(args) > 2 && args[2] == "addr":
			return `[{"ifname":"ens18","addr_info":[{"local":"192.168.1.50","prefixlen":24}]}]`, nil
		case name == "ip":
			return `[{"gateway":"192.168.1.1"}]`, nil
		}
		return "", nil
	}}
	return s, &calls
}

func TestApplyStatic(t *testing.T) {
	s, calls := fakeSystem(t, true)
	err := s.Apply(context.Background(), Config{Address: "192.168.1.20/24", Gateway: "192.168.1.1", DNS: []string{"192.168.1.1"}, Hostname: "backup01"}, true)
	if err != nil {
		t.Fatal(err)
	}
	st, _ := s.Current(context.Background())
	if st.DHCP || st.Address != "192.168.1.20/24" || st.Hostname != "backup01" || !st.Managed {
		t.Errorf("%+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, "etc/network/interfaces")); strings.Contains(string(b), "ens18") {
		t.Error("ifupdown still configures the interface")
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, "etc/hosts")); !strings.Contains(string(b), "127.0.1.1\tbackup01") || strings.Contains(string(b), "old") {
		t.Errorf("hosts: %s", b)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, "etc/resolv.conf")); !strings.Contains(string(b), "nameserver 192.168.1.1") {
		t.Errorf("resolv.conf: %s", b)
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "systemctl restart systemd-networkd.service") {
		t.Errorf("not activated: %v", *calls)
	}
}

func TestApplyRevertsWithoutGateway(t *testing.T) {
	s, _ := fakeSystem(t, false)
	os.MkdirAll(filepath.Join(s.Root, "etc/systemd/network"), 0o755)
	os.WriteFile(filepath.Join(s.Root, NetworkFile), []byte(networkFile(Config{DHCP: true})), 0o644)
	err := s.Apply(context.Background(), Config{Address: "192.168.1.20/24", Gateway: "192.168.1.254", DNS: []string{"192.168.1.1"}}, true)
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("want a revert, got %v", err)
	}
	if st, _ := s.Current(context.Background()); !st.DHCP {
		t.Errorf("previous settings not restored: %+v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, "etc/resolv.conf")); !strings.Contains(string(b), "9.9.9.9") {
		t.Errorf("resolv.conf not restored: %s", b)
	}
}

func TestSSH(t *testing.T) {
	s, calls := fakeSystem(t, true)
	os.MkdirAll(filepath.Join(s.Root, "etc/ssh/sshd_config.d"), 0o755)
	for _, bad := range []SSHConfig{{Enabled: true, Port: 8443}, {Enabled: true, Port: 70000}} {
		if err := s.ApplySSH(context.Background(), bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
	if err := s.ApplySSH(context.Background(), SSHConfig{Enabled: true, Port: 2222}); err != nil {
		t.Fatal(err)
	}
	if got := s.SSH(context.Background()); got.Port != 2222 || !got.Enabled {
		t.Errorf("%+v", got)
	}
	if err := s.ApplySSH(context.Background(), SSHConfig{Enabled: false, Port: 22}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.Root, SSHPortFile)); err == nil {
		t.Error("port file kept for port 22")
	}
	if !strings.Contains(strings.Join(*calls, "\n"), "systemctl disable --now ssh.service") {
		t.Errorf("not stopped: %v", *calls)
	}
}
