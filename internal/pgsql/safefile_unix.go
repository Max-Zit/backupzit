//go:build !windows

package pgsql

import "syscall"

const oNoFollow = syscall.O_NOFOLLOW
