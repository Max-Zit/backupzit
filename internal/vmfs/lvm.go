package vmfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// lvmLabel returns the offset of the LVM2 label in the first sectors, or -1.
func lvmLabel(b []byte) int {
	for s := 0; s < 4; s++ {
		off := s * 512
		if len(b) >= off+32 && string(b[off:off+8]) == "LABELONE" && string(b[off+24:off+32]) == "LVM2 001" {
			return off
		}
	}
	return -1
}

// lvmVolumes reads the VG metadata of a physical volume and returns its
// linear logical volumes. Volume groups spanning several disks are not
// supported (their other extents are in another disk).
func lvmVolumes(pv Volume) ([]Volume, error) {
	b := make([]byte, 4096)
	if _, err := pv.r.ReadAt(b, 0); err != nil && err != io.EOF {
		return nil, err
	}
	lo := lvmLabel(b)
	if lo < 0 {
		return nil, fmt.Errorf("no LVM label")
	}
	ph := lo + int(binary.LittleEndian.Uint32(b[lo+20:]))
	pvUUID := string(b[ph : ph+32])
	p := ph + 32 + 8
	// Data areas, then metadata areas, each list ends with a zero entry.
	for binary.LittleEndian.Uint64(b[p:]) != 0 {
		p += 16
	}
	p += 16
	var text string
	for ; binary.LittleEndian.Uint64(b[p:]) != 0; p += 16 {
		mdaOff := int64(binary.LittleEndian.Uint64(b[p:]))
		t, err := readMetadata(pv.r, mdaOff)
		if err == nil {
			text = t
			break
		}
	}
	if text == "" {
		return nil, fmt.Errorf("no LVM metadata")
	}
	cfg, err := parseLVMConfig(text)
	if err != nil {
		return nil, err
	}
	var vgName string
	var vg map[string]any
	for k, v := range cfg {
		if m, ok := v.(map[string]any); ok {
			if _, ok := m["physical_volumes"]; ok {
				vgName, vg = k, m
			}
		}
	}
	if vg == nil {
		return nil, fmt.Errorf("no volume group in the metadata")
	}
	extent := toInt(vg["extent_size"]) * 512
	pvs, _ := vg["physical_volumes"].(map[string]any)
	var thisPV string
	var peStart int64
	for name, v := range pvs {
		m, _ := v.(map[string]any)
		if strings.ReplaceAll(fmt.Sprint(m["id"]), "-", "") == pvUUID {
			thisPV, peStart = name, toInt(m["pe_start"])*512
		}
	}
	if thisPV == "" {
		return nil, fmt.Errorf("physical volume not found in the metadata")
	}
	lvs, _ := vg["logical_volumes"].(map[string]any)
	var names []string
	for n := range lvs {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []Volume
	for _, n := range names {
		lv, _ := lvs[n].(map[string]any)
		segs := []segment{}
		ok := true
		for i := 1; i <= int(toInt(lv["segment_count"])); i++ {
			s, _ := lv["segment"+strconv.Itoa(i)].(map[string]any)
			stripes, _ := s["stripes"].([]any)
			if fmt.Sprint(s["type"]) != "striped" || toInt(s["stripe_count"]) != 1 || len(stripes) != 2 || fmt.Sprint(stripes[0]) != thisPV {
				ok = false
				break
			}
			segs = append(segs, segment{
				start: toInt(s["start_extent"]) * extent, length: toInt(s["extent_count"]) * extent,
				pvOff: peStart + toInt(stripes[1])*extent,
			})
		}
		if !ok || len(segs) == 0 {
			continue // thin, mirrored, striped or on another disk
		}
		var size int64
		for _, s := range segs {
			size = max(size, s.start+s.length)
		}
		v := Volume{ID: "lvm/" + vgName + "/" + n, Name: "LVM " + vgName + "/" + n, Size: size, r: &lvReader{pv: pv.r, segs: segs, size: size}}
		detect(&v)
		out = append(out, v)
	}
	return out, nil
}

func readMetadata(r io.ReaderAt, mdaOff int64) (string, error) {
	h := make([]byte, 512)
	if _, err := r.ReadAt(h, mdaOff); err != nil {
		return "", err
	}
	if string(h[4:20]) != " LVM2 x[5A%r0N*>" {
		return "", fmt.Errorf("bad metadata header")
	}
	size := int64(binary.LittleEndian.Uint64(h[32:]))
	off := int64(binary.LittleEndian.Uint64(h[40:]))
	n := int64(binary.LittleEndian.Uint64(h[48:]))
	if n <= 0 || n > 16<<20 {
		return "", fmt.Errorf("no metadata")
	}
	buf := make([]byte, n)
	first := min(n, size-off) // circular buffer
	if _, err := r.ReadAt(buf[:first], mdaOff+off); err != nil {
		return "", err
	}
	if first < n {
		// The ring buffer continues after the 512 byte header.
		if _, err := r.ReadAt(buf[first:], mdaOff+512); err != nil {
			return "", err
		}
	}
	return strings.TrimRight(string(buf), "\x00"), nil
}

type segment struct{ start, length, pvOff int64 }

type lvReader struct {
	pv   io.ReaderAt
	segs []segment
	size int64
}

func (l *lvReader) ReadAt(b []byte, off int64) (int, error) {
	n := 0
	for n < len(b) {
		pos := off + int64(n)
		if pos >= l.size {
			return n, io.EOF
		}
		var seg *segment
		for i := range l.segs {
			if pos >= l.segs[i].start && pos < l.segs[i].start+l.segs[i].length {
				seg = &l.segs[i]
			}
		}
		if seg == nil {
			return n, fmt.Errorf("offset %d outside the logical volume", pos)
		}
		chunk := min(int64(len(b)-n), seg.start+seg.length-pos)
		k, err := l.pv.ReadAt(b[n:n+int(chunk)], seg.pvOff+pos-seg.start)
		n += k
		if err != nil && !(err == io.EOF && k == int(chunk)) {
			return n, err
		}
	}
	return n, nil
}

func toInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

// parseLVMConfig parses the LVM text metadata format into nested maps:
// sections {..}, key = value, arrays [..], strings and integers.
func parseLVMConfig(s string) (map[string]any, error) {
	p := &lvmParser{s: s}
	return p.section(false)
}

type lvmParser struct {
	s   string
	pos int
}

func (p *lvmParser) skip() {
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		switch {
		case c == '#':
			for p.pos < len(p.s) && p.s[p.pos] != '\n' {
				p.pos++
			}
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			p.pos++
		default:
			return
		}
	}
}

func (p *lvmParser) ident() string {
	start := p.pos
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if c == '=' || c == '{' || c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '}' {
			break
		}
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *lvmParser) section(nested bool) (map[string]any, error) {
	m := map[string]any{}
	for {
		p.skip()
		if p.pos >= len(p.s) {
			if nested {
				return nil, fmt.Errorf("unexpected end of LVM metadata")
			}
			return m, nil
		}
		if p.s[p.pos] == '}' {
			p.pos++
			return m, nil
		}
		key := p.ident()
		if key == "" {
			return nil, fmt.Errorf("syntax error in LVM metadata at %d", p.pos)
		}
		p.skip()
		if p.pos >= len(p.s) {
			return nil, fmt.Errorf("unexpected end of LVM metadata")
		}
		switch p.s[p.pos] {
		case '{':
			p.pos++
			sub, err := p.section(true)
			if err != nil {
				return nil, err
			}
			m[key] = sub
		case '=':
			p.pos++
			p.skip()
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			m[key] = v
		default:
			return nil, fmt.Errorf("syntax error in LVM metadata at %d", p.pos)
		}
	}
}

func (p *lvmParser) value() (any, error) {
	if p.pos >= len(p.s) {
		return nil, fmt.Errorf("unexpected end of LVM metadata")
	}
	switch c := p.s[p.pos]; {
	case c == '"':
		p.pos++
		var b strings.Builder
		for p.pos < len(p.s) && p.s[p.pos] != '"' {
			if p.s[p.pos] == '\\' && p.pos+1 < len(p.s) {
				p.pos++
			}
			b.WriteByte(p.s[p.pos])
			p.pos++
		}
		p.pos++
		return b.String(), nil
	case c == '[':
		p.pos++
		var arr []any
		for {
			p.skip()
			if p.pos >= len(p.s) {
				return nil, fmt.Errorf("unexpected end of LVM metadata")
			}
			if p.s[p.pos] == ']' {
				p.pos++
				return arr, nil
			}
			if p.s[p.pos] == ',' {
				p.pos++
				continue
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
	default:
		start := p.pos
		for p.pos < len(p.s) && (p.s[p.pos] == '-' || p.s[p.pos] == '.' || (p.s[p.pos] >= '0' && p.s[p.pos] <= '9')) {
			p.pos++
		}
		if start == p.pos {
			return nil, fmt.Errorf("syntax error in LVM metadata at %d", p.pos)
		}
		n, err := strconv.ParseInt(p.s[start:p.pos], 10, 64)
		if err != nil {
			return p.s[start:p.pos], nil
		}
		return n, nil
	}
}
