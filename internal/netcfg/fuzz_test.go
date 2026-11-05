package netcfg

import "testing"

// The network file is written by the console and may be edited by hand.
func FuzzParseNetworkFile(f *testing.F) {
	f.Add(networkFile(Config{Address: "192.168.1.20/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}))
	f.Add(networkFile(Config{DHCP: true}))
	f.Add("[Network]\nAddress=\nDNS=a b\n")
	f.Fuzz(func(t *testing.T, s string) {
		_ = parseNetworkFile(s)
	})
}
