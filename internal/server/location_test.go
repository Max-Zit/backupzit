package server

import "testing"

func TestStorageLocations(t *testing.T) {
	if got, err := smbLocation(` \nas01\ `, `\Backups\`, `\backupzit\office\`, "CORP;backup"); err != nil || got != "smb://CORP;backup@nas01/Backups/backupzit/office" {
		t.Errorf("smb: %q %v", got, err)
	}
	if _, err := smbLocation("nas01", "", "", "u"); err == nil {
		t.Error("smb without share accepted")
	}
	if got, err := s3Location("https://s3.wasabisys.com/", "/bkt/", "", false); err != nil || got != "s3://s3.wasabisys.com/bkt" {
		t.Errorf("s3: %q %v", got, err)
	}
	if got, _ := s3Location("10.0.0.5:9000", "b1", "x/y", true); got != "s3://10.0.0.5:9000/b1/x/y?tls=false" {
		t.Errorf("s3 http: %q", got)
	}
}
