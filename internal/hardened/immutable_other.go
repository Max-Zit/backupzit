//go:build !linux

package hardened

// The hardened repository runs on Linux; elsewhere (tests, development)
// immutability is only enforced by the service itself.
func setImmutable(path string, on bool) error { return nil }

func immutableSupported() bool { return false }
