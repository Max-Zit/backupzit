package hyperv

import (
	"strings"
	"testing"
)

func TestOnCSV(t *testing.T) {
	for dir, want := range map[string]bool{
		`C:\ClusterStorage\Volume1\VMs`: true, `c:\clusterstorage\volume2`: true, `C:/ClusterStorage/Volume1`: true,
		`D:\VMs`: false, `C:\ProgramData\Microsoft\Windows\Hyper-V`: false, `C:\ClusterStorageOld\x`: false,
	} {
		if got := onCSV(dir); got != want {
			t.Errorf("%s: %v", dir, got)
		}
	}
}

func TestTPMNote(t *testing.T) {
	if n := tpmNote("TPM:kept"); !strings.Contains(n, "restored from the backup") {
		t.Error(n)
	}
	if n := tpmNote("TPM:new:The operation failed."); !strings.Contains(n, "guardian key of the original host") || !strings.Contains(n, "recovery key") {
		t.Error(n)
	}
	if n := tpmNote("TPM:new:the backup has no vTPM state (made before BackupZit 0.48)"); !strings.Contains(n, "made before BackupZit 0.48") {
		t.Error(n)
	}
}
