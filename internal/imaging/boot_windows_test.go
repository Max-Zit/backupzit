//go:build windows

package imaging

import "testing"

func TestDetectFirmware(t *testing.T) {
	fw := DetectFirmware()
	t.Logf("firmware: %s", fw)
	if fw.Type == FirmwareUnknown {
		t.Error("firmware type of a Windows machine should be known")
	}
}
