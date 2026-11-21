package imaging

import "testing"

func TestParsePnputilDevices(t *testing.T) {
	out := "Microsoft PnP Utility\r\n\r\n" +
		"Instance ID:                PCI\\VEN_8086&DEV_467F&SUBSYS_00000000&REV_00\\3&11583659&0&70\r\n" +
		"Device Description:         RAID Controller\r\n" +
		"Class Name:                 Unknown\r\n" +
		"Class GUID:                 {00000000-0000-0000-0000-000000000000}\r\n" +
		"Manufacturer Name:          \r\n" +
		"Status:                     Problem\r\n" +
		"Problem Code:               28 (0x1C) [CM_PROB_FAILED_INSTALL]\r\n" +
		"\r\n" +
		"Instanz-ID:                 PCI\\VEN_1AF4&DEV_1001\\3&267A616A&0&20\r\n" +
		"Gerätebeschreibung:         SCSI-Controller\r\n" +
		"Klassenname:                SCSIAdapter\r\n" +
		"Status:                     Problem\r\n" +
		"Problemcode:                28 (0x1C) [CM_PROB_FAILED_INSTALL]\r\n"
	devs := parsePnputilDevices(out)
	if len(devs) != 2 {
		t.Fatalf("got %d devices: %+v", len(devs), devs)
	}
	if devs[0].Description != "RAID Controller" || devs[0].Problem == "" {
		t.Errorf("first: %+v", devs[0])
	}
	if devs[1].Class != "SCSIAdapter" || devs[1].InstanceID != `PCI\VEN_1AF4&DEV_1001\3&267A616A&0&20` {
		t.Errorf("second: %+v", devs[1])
	}
	if len(parsePnputilDevices("Microsoft PnP Utility\r\n\r\nNo devices were found on the system.\r\n")) != 0 {
		t.Error("devices found in an empty listing")
	}
}
