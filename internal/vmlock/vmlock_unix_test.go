//go:build linux || darwin || freebsd

package vmlock

import "time"

func init() { pollEvery = 10 * time.Millisecond }
