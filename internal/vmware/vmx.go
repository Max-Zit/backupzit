package vmware

import (
	"bytes"
	"strings"
)

// vmx is a VM configuration file: `key = "value"` lines in order.
type vmx struct {
	keys []string
	vals map[string]string // by lower-case key
}

func parseVMX(b []byte) *vmx {
	v := &vmx{vals: map[string]string{}}
	for _, line := range strings.Split(strings.ReplaceAll(string(b), "\r\n", "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		val = strings.TrimSpace(val)
		val = strings.TrimSuffix(strings.TrimPrefix(val, `"`), `"`)
		v.Set(k, unescapeVMX(val))
	}
	return v
}

// VMX values escape special characters as |XX (hex).
func unescapeVMX(s string) string {
	if !strings.Contains(s, "|") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '|' && i+2 < len(s) {
			if n, ok := hexByte(s[i+1 : i+3]); ok {
				b.WriteByte(n)
				i += 2
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func hexByte(s string) (byte, bool) {
	var n byte
	for _, c := range []byte(s) {
		switch {
		case c >= '0' && c <= '9':
			n = n<<4 | (c - '0')
		case c >= 'a' && c <= 'f':
			n = n<<4 | (c - 'a' + 10)
		case c >= 'A' && c <= 'F':
			n = n<<4 | (c - 'A' + 10)
		default:
			return 0, false
		}
	}
	return n, true
}

func escapeVMX(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		if c == '"' || c == '|' || c < 0x20 {
			b.WriteString("|" + strings.ToUpper(string("0123456789abcdef"[c>>4])+string("0123456789abcdef"[c&15])))
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

func (v *vmx) Get(k string) string { return v.vals[strings.ToLower(k)] }

func (v *vmx) Has(k string) bool { _, ok := v.vals[strings.ToLower(k)]; return ok }

func (v *vmx) Set(k, val string) {
	lk := strings.ToLower(k)
	if _, ok := v.vals[lk]; !ok {
		v.keys = append(v.keys, k)
	}
	v.vals[lk] = val
}

func (v *vmx) Delete(k string) {
	lk := strings.ToLower(k)
	if _, ok := v.vals[lk]; !ok {
		return
	}
	delete(v.vals, lk)
	for i, x := range v.keys {
		if strings.ToLower(x) == lk {
			v.keys = append(v.keys[:i], v.keys[i+1:]...)
			return
		}
	}
}

// DeletePrefix removes all keys starting with prefix (case-insensitive).
func (v *vmx) DeletePrefix(prefix string) {
	p := strings.ToLower(prefix)
	for _, k := range append([]string(nil), v.keys...) {
		if strings.HasPrefix(strings.ToLower(k), p) {
			v.Delete(k)
		}
	}
}

// Keys returns the keys in file order.
func (v *vmx) Keys() []string { return append([]string(nil), v.keys...) }

func (v *vmx) Bytes() []byte {
	var b bytes.Buffer
	for _, k := range v.keys {
		b.WriteString(k + ` = "` + escapeVMX(v.vals[strings.ToLower(k)]) + "\"\n")
	}
	return b.Bytes()
}
