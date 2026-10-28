package server

import (
	"sync"
	"time"
)

// Agents send their own time with every poll (0.44.0 and newer). A clock
// that is off makes backups to S3 and Azure fail (requests are refused
// when the time differs by more than 15 minutes) and schedules run at the
// wrong hour, so the console warns before that happens.

// clockSkewWarn is the offset from which the console warns.
const clockSkewWarn = 2 * time.Minute

type clockTracker struct {
	mu sync.Mutex
	m  map[int64]clockEntry
}

type clockEntry struct {
	offset time.Duration // agent time minus console time
	at     time.Time
}

// seen records the time an agent reported (unix milliseconds) at now.
func (c *clockTracker) seen(agentID, agentMillis int64, now time.Time) {
	if agentMillis <= 0 {
		return // older agent
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[int64]clockEntry{}
	}
	c.m[agentID] = clockEntry{offset: time.UnixMilli(agentMillis).Sub(now), at: now}
}

// skewed lists the agents whose clock is off by clockSkewWarn or more,
// from polls of the last 15 minutes.
func (c *clockTracker) skewed(now time.Time) map[int64]time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[int64]time.Duration{}
	for id, e := range c.m {
		if now.Sub(e.at) > 15*time.Minute {
			continue
		}
		if e.offset >= clockSkewWarn || e.offset <= -clockSkewWarn {
			out[id] = e.offset
		}
	}
	return out
}

// skewText says how far and in which direction a clock is off.
func skewText(l *language, d time.Duration) string {
	ahead := d > 0
	if d < 0 {
		d = -d
	}
	n := humanAge(l, d)
	if ahead {
		return l.T("the clock of this computer is %s ahead of the console — turn on time synchronization there (S3 and Azure refuse requests from 15 minutes on)", n)
	}
	return l.T("the clock of this computer is %s behind the console — turn on time synchronization there (S3 and Azure refuse requests from 15 minutes on)", n)
}

// skewShort is the offset for the agents table, e.g. "+12 min".
func skewShort(l *language, d time.Duration) string {
	if d < 0 {
		return "−" + humanAge(l, -d)
	}
	return "+" + humanAge(l, d)
}
