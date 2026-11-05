package vmware

import "testing"

// .vmx files and datastore paths come from the ESXi host.
func FuzzParseVMX(f *testing.F) {
	f.Add([]byte(".encoding = \"UTF-8\"\ndisplayName = \"web |22 1\"\nscsi0:0.fileName = \"web.vmdk\"\n"))
	f.Add([]byte("a = \"|ZZ\"\n#c\nb=\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		v := parseVMX(b)
		w := parseVMX(v.Bytes())
		for _, k := range v.Keys() {
			if v.Get(k) != w.Get(k) {
				t.Fatalf("%q: %q became %q after writing", k, v.Get(k), w.Get(k))
			}
		}
	})
}

func FuzzParseDSPath(f *testing.F) {
	f.Add("[datastore1] web/web.vmx")
	f.Add("[] ")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseDSPath(s)
	})
}
