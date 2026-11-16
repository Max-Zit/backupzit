package hardened

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// usageMaxAge is how long a measured disk usage is trusted before the
// directory is walked again. In between, uploads are added as they happen;
// space freed by the hourly sweep shows up at the next measurement.
const usageMaxAge = 10 * time.Minute

// keyState holds the counters of one access key: the limits' bookkeeping
// and the values exported on /metrics. Counters start at zero when the
// service starts, as Prometheus counters do.
type keyState struct {
	day      string // UTC date of dayBytes
	dayBytes int64
	pending  int64 // bytes of uploads in progress, reserved against the limits

	window      time.Time // start of the current one-minute window
	windowCount int

	puts, putBytes, deletes, keeps, extended, reads int64
	refused                                         map[string]int64
}

type usageState struct {
	mu      sync.Mutex
	bytes   int64
	at      time.Time
	pending int64
}

type limiter struct {
	now func() time.Time

	mu    sync.Mutex
	keys  map[string]*keyState
	usage map[string]*usageState
}

func (l *limiter) state(name string) *keyState {
	if l.keys == nil {
		l.keys = map[string]*keyState{}
	}
	st := l.keys[name]
	if st == nil {
		st = &keyState{refused: map[string]int64{}}
		l.keys[name] = st
	}
	return st
}

func (l *limiter) clock() time.Time {
	if l.now != nil {
		return l.now()
	}
	return time.Now()
}

// refuse counts a refused request of the key.
func (l *limiter) refuse(key, reason string) {
	l.mu.Lock()
	l.state(key).refused[reason]++
	l.mu.Unlock()
}

// rate counts a write request against the key's per-minute limit and
// returns how long to wait when the limit is reached.
func (l *limiter) rate(k Key) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	st := l.state(k.Name)
	if k.Rate <= 0 {
		return 0, true
	}
	now := l.clock()
	if now.Sub(st.window) >= time.Minute {
		st.window, st.windowCount = now, 0
	}
	if st.windowCount >= k.Rate {
		st.refused["rate"]++
		return st.window.Add(time.Minute).Sub(now), false
	}
	st.windowCount++
	return 0, true
}

// errLimit is a refused upload: status 507 with a message for the client.
type errLimit struct{ msg string }

func (e errLimit) Error() string { return e.msg }

// reserve checks an upload of size bytes against the key's daily and
// storage limits and reserves the space until done is called with whether
// a new file was stored.
func (l *limiter) reserve(k Key, size int64, used func(prefix string) (int64, error)) (done func(created bool), err error) {
	now := l.clock()
	today := now.UTC().Format("2006-01-02")
	l.mu.Lock()
	st := l.state(k.Name)
	if st.day != today {
		st.day, st.dayBytes = today, 0
	}
	if k.Daily > 0 && st.dayBytes+st.pending+size > k.Daily {
		st.refused["daily"]++
		l.mu.Unlock()
		return nil, errLimit{fmt.Sprintf("daily upload limit of key %q reached (%s per day)", k.Name, humanBytes(k.Daily))}
	}
	st.pending += size
	l.mu.Unlock()
	release := func() {
		l.mu.Lock()
		st.pending -= size
		l.mu.Unlock()
	}

	var us *usageState
	if k.Quota > 0 {
		l.mu.Lock()
		if l.usage == nil {
			l.usage = map[string]*usageState{}
		}
		us = l.usage[k.Path]
		if us == nil {
			us = &usageState{}
			l.usage[k.Path] = us
		}
		l.mu.Unlock()
		us.mu.Lock()
		if us.at.IsZero() || now.Sub(us.at) > usageMaxAge {
			n, err := used(k.Path)
			if err != nil {
				us.mu.Unlock()
				release()
				return nil, err
			}
			us.bytes, us.at = n, now
		}
		if us.bytes+us.pending+size > k.Quota {
			us.mu.Unlock()
			release()
			l.refuse(k.Name, "quota")
			return nil, errLimit{fmt.Sprintf("storage quota of key %q reached (%s)", k.Name, humanBytes(k.Quota))}
		}
		us.pending += size
		us.mu.Unlock()
	}
	return func(created bool) {
		l.mu.Lock()
		st.pending -= size
		if created {
			st.dayBytes += size
			st.puts++
			st.putBytes += size
		}
		l.mu.Unlock()
		if us != nil {
			us.mu.Lock()
			us.pending -= size
			if created {
				us.bytes += size
			}
			us.mu.Unlock()
		}
	}, nil
}

func (l *limiter) count(key string, f func(*keyState)) {
	l.mu.Lock()
	f(l.state(key))
	l.mu.Unlock()
}

// writeMetrics writes the Prometheus text format. Keys are labelled by
// name only, never by their secret.
func (l *limiter) writeMetrics(w http.ResponseWriter, keys []Key, lockDays int, used func(prefix string) (int64, error)) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	var b strings.Builder
	metric := func(name, typ, help string) {
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", name, help, name, typ)
	}
	q := strconv.Quote
	metric("backupzit_repo_lock_days", "gauge", "Immutability period of stored files in days.")
	fmt.Fprintf(&b, "backupzit_repo_lock_days %d\n", lockDays)

	today := l.clock().UTC().Format("2006-01-02")
	l.mu.Lock()
	type row struct {
		k  Key
		st keyState
	}
	rows := make([]row, 0, len(keys))
	for _, k := range keys {
		st := *l.state(k.Name)
		st.refused = map[string]int64{}
		for r, n := range l.state(k.Name).refused {
			st.refused[r] = n
		}
		if st.day != today {
			st.dayBytes = 0
		}
		rows = append(rows, row{k, st})
	}
	l.mu.Unlock()

	counters := []struct {
		name, help string
		v          func(keyState) int64
	}{
		{"backupzit_repo_uploads_total", "New files stored with the key.", func(s keyState) int64 { return s.puts }},
		{"backupzit_repo_uploaded_bytes_total", "Bytes of new files stored with the key.", func(s keyState) int64 { return s.putBytes }},
		{"backupzit_repo_deletes_total", "Delete requests of the key (files are hidden until their period ends).", func(s keyState) int64 { return s.deletes }},
		{"backupzit_repo_keep_requests_total", "Requests of the key to extend retention.", func(s keyState) int64 { return s.keeps }},
		{"backupzit_repo_kept_files_total", "Files whose retention was extended for the key.", func(s keyState) int64 { return s.extended }},
		{"backupzit_repo_reads_total", "Files and lists read with the key.", func(s keyState) int64 { return s.reads }},
	}
	for _, c := range counters {
		metric(c.name, "counter", c.help)
		for _, r := range rows {
			fmt.Fprintf(&b, "%s{key=%s} %d\n", c.name, q(r.k.Name), c.v(r.st))
		}
	}
	metric("backupzit_repo_refused_total", "counter", "Requests of the key refused, by reason (path, exists, quota, daily, rate).")
	for _, r := range rows {
		reasons := make([]string, 0, len(r.st.refused))
		for reason := range r.st.refused {
			reasons = append(reasons, reason)
		}
		sort.Strings(reasons)
		for _, reason := range reasons {
			fmt.Fprintf(&b, "backupzit_repo_refused_total{key=%s,reason=%s} %d\n", q(r.k.Name), q(reason), r.st.refused[reason])
		}
	}
	metric("backupzit_repo_uploaded_bytes_today", "gauge", "Bytes of new files stored with the key today (UTC).")
	for _, r := range rows {
		fmt.Fprintf(&b, "backupzit_repo_uploaded_bytes_today{key=%s} %d\n", q(r.k.Name), r.st.dayBytes)
	}
	limits := []struct {
		name, help string
		v          func(Key) int64
	}{
		{"backupzit_repo_quota_bytes", "Storage quota of the key (0: none).", func(k Key) int64 { return k.Quota }},
		{"backupzit_repo_daily_limit_bytes", "Daily upload limit of the key (0: none).", func(k Key) int64 { return k.Daily }},
		{"backupzit_repo_rate_limit_per_minute", "Write requests per minute allowed for the key (0: no limit).", func(k Key) int64 { return int64(k.Rate) }},
	}
	for _, m := range limits {
		metric(m.name, "gauge", m.help)
		for _, r := range rows {
			fmt.Fprintf(&b, "%s{key=%s} %d\n", m.name, q(r.k.Name), m.v(r.k))
		}
	}
	metric("backupzit_repo_used_bytes", "gauge", "Disk space used below the key's path, deleted but still retained files included (keys with a path or quota).")
	for _, r := range rows {
		if r.k.Path == "" && r.k.Quota <= 0 {
			continue
		}
		if n, err := l.measured(r.k.Path, used); err == nil {
			fmt.Fprintf(&b, "backupzit_repo_used_bytes{key=%s,path=%s} %d\n", q(r.k.Name), q(r.k.Path), n)
		}
	}
	_, _ = w.Write([]byte(b.String()))
}

// measured returns the usage below prefix, measuring when it is stale.
func (l *limiter) measured(prefix string, used func(string) (int64, error)) (int64, error) {
	l.mu.Lock()
	if l.usage == nil {
		l.usage = map[string]*usageState{}
	}
	us := l.usage[prefix]
	if us == nil {
		us = &usageState{}
		l.usage[prefix] = us
	}
	l.mu.Unlock()
	us.mu.Lock()
	defer us.mu.Unlock()
	now := l.clock()
	if us.at.IsZero() || now.Sub(us.at) > usageMaxAge {
		n, err := used(prefix)
		if err != nil {
			return 0, err
		}
		us.bytes, us.at = n, now
	}
	return us.bytes, nil
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// ParseSize reads a size such as 500G, 2T, 750M or a plain number of bytes
// (binary units: 1G = 1024^3 bytes).
func ParseSize(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(strings.TrimSuffix(s, "B"), "I")
	mult := int64(1)
	if s != "" {
		if i := strings.IndexByte("KMGTP", s[len(s)-1]); i >= 0 {
			mult = int64(1) << (10 * (i + 1))
			s = s[:len(s)-1]
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q (e.g. 500G, 2T)", s)
	}
	return int64(v * float64(mult)), nil
}
