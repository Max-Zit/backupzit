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

func TestGUIDBytesMatchWindows(t *testing.T) {
	for _, s := range []string{GPTTypeEFI, GPTTypeBasicData, "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"} {
		a, err1 := guidBytes(s)
		b, err2 := guidBytesPortable(s)
		if err1 != nil || err2 != nil || a != b {
			t.Errorf("%s: % x vs % x (%v, %v)", s, a, b, err1, err2)
		}
	}
}
