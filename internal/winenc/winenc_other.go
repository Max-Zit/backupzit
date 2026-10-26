//go:build !windows

package winenc

func fromCodePage(b []byte) string { return string(b) }
