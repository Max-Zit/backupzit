//go:build !(linux || darwin || freebsd)

package vmlock

import "context"

// lockFile: on Windows the Hyper-V code has its own named mutex; VMware
// proxies there are serialized within the agent's process.
func lockFile(context.Context, string) (func(), error) { return func() {}, nil }
