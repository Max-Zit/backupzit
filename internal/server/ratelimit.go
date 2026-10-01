package server

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginLimiter slows down password guessing: after maxFails failed
// sign-ins within window from one address, or for one username, further
// attempts are refused until the window has passed. A username tolerates
// more failures than an address, so a guesser cannot easily lock out the
// administrator.
type loginLimiter struct {
	maxFails, maxUserFails int
	window                 time.Duration
	now                    func() time.Time

	mu    sync.Mutex
	fails map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{maxFails: 5, maxUserFails: 20, window: 15 * time.Minute, now: time.Now, fails: map[string][]time.Time{}}
}

func loginKeys(r *http.Request, username string) []string {
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	return []string{"ip:" + ip, "user:" + strings.ToLower(strings.TrimSpace(username))}
}

// recent drops failures older than the window and returns the rest.
func (l *loginLimiter) recent(key string) []time.Time {
	cut := l.now().Add(-l.window)
	f := l.fails[key]
	i := 0
	for i < len(f) && f[i].Before(cut) {
		i++
	}
	f = f[i:]
	if len(f) == 0 {
		delete(l.fails, key)
		return nil
	}
	l.fails[key] = f
	return f
}

// Blocked returns how long the caller has to wait, or 0.
func (l *loginLimiter) Blocked(keys []string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	var wait time.Duration
	for _, k := range keys {
		max := l.maxFails
		if strings.HasPrefix(k, "user:") {
			max = l.maxUserFails
		}
		if f := l.recent(k); len(f) >= max {
			if w := f[len(f)-max].Add(l.window).Sub(l.now()); w > wait {
				wait = w
			}
		}
	}
	return wait
}

func (l *loginLimiter) Fail(keys []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.fails) > 10000 { // bound memory under a distributed attack
		for k := range l.fails {
			l.recent(k)
		}
	}
	now := l.now()
	for _, k := range keys {
		l.fails[k] = append(l.recent(k), now)
	}
}

func (l *loginLimiter) Success(keys []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, k := range keys {
		delete(l.fails, k)
	}
}
