package imaging

import "testing"

func TestBootFilesFor(t *testing.T) {
	cases := []struct {
		fw    Firmware
		style string
		files string
		warn  bool
	}{
		{Firmware{Type: FirmwareUEFI}, StyleGPT, "UEFI", false},
		{Firmware{Type: FirmwareBIOS}, StyleMBR, "BIOS", false},
		{Firmware{Type: FirmwareUEFI}, StyleMBR, "ALL", true},
		{Firmware{Type: FirmwareUEFI, SecureBoot: true}, StyleMBR, "ALL", true},
		{Firmware{Type: FirmwareBIOS}, StyleGPT, "UEFI", true},
		{Firmware{}, StyleMBR, "BIOS", false},
		{Firmware{}, StyleGPT, "UEFI", false},
	}
	for _, c := range cases {
		files, warn := BootFilesFor(c.fw, c.style)
		if files != c.files || (warn != "") != c.warn {
			t.Errorf("BootFilesFor(%+v, %s) = %q, %q", c.fw, c.style, files, warn)
		}
	}
}

func TestValidRecoveryKey(t *testing.T) {
	if !ValidRecoveryKey("123456-234567-345678-456789-567890-678901-789012-890123") {
		t.Error("valid key refused")
	}
	for _, k := range []string{"", "123456", "123456-234567-345678-456789-567890-678901-789012-89012x", "1234567-234567-345678-456789-567890-678901-789012-890123"} {
		if ValidRecoveryKey(k) {
			t.Errorf("%q accepted", k)
		}
	}
}
