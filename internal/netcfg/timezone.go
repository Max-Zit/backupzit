package netcfg

import (
	"context"
	"errors"
	"os"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata" // validate zone names also where the system has no zoneinfo
)

// LocaltimeFile is the link that names the system time zone.
const LocaltimeFile = "/etc/localtime"

var zoneRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_+-]*(/[A-Za-z0-9_+-]+){0,2}$`)

// ValidTimeZone checks an IANA time zone name such as Europe/Belgrade.
func ValidTimeZone(name string) error {
	if name == "" || name == "Local" || !zoneRe.MatchString(name) {
		return errors.New("enter a time zone such as Europe/Belgrade, Europe/Berlin or UTC")
	}
	if _, err := time.LoadLocation(name); err != nil {
		return errors.New("unknown time zone " + name + "; use a name such as Europe/Belgrade")
	}
	return nil
}

// TimeZone is the system time zone (UTC when not set).
func (s *System) TimeZone() string {
	target, err := os.Readlink(s.path(LocaltimeFile))
	if err != nil {
		return "UTC"
	}
	if _, z, ok := strings.Cut(target, "zoneinfo/"); ok && z != "" {
		return z
	}
	return "UTC"
}

// ApplyTimeZone sets the system time zone. The console reads the zone when
// it starts, so it is restarted afterwards (restart false: the caller does).
func (s *System) ApplyTimeZone(ctx context.Context, name string, restart bool) error {
	if err := ValidTimeZone(name); err != nil {
		return err
	}
	if _, err := s.Run(ctx, "timedatectl", "set-timezone", name); err != nil {
		return err
	}
	if restart {
		_, err := s.Run(ctx, "systemctl", "restart", "backupzit-server")
		return err
	}
	return nil
}
