package fsutil

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

// setDACL gives p exactly the entries (no inherited ones).
func setDACL(t *testing.T, p string, entries []windows.EXPLICIT_ACCESS) {
	t.Helper()
	acl, err := windows.ACLFromEntries(entries, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Skipf("cannot set the access list: %v", err)
	}
}

func allow(t *testing.T, sid windows.WELL_KNOWN_SID_TYPE, mask windows.ACCESS_MASK) windows.EXPLICIT_ACCESS {
	t.Helper()
	s, err := windows.CreateWellKnownSid(sid)
	if err != nil {
		t.Fatal(err)
	}
	return windows.EXPLICIT_ACCESS{AccessPermissions: mask, AccessMode: windows.GRANT_ACCESS, Inheritance: windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{TrusteeForm: windows.TRUSTEE_IS_SID, TrusteeType: windows.TRUSTEE_IS_WELL_KNOWN_GROUP, TrusteeValue: windows.TrusteeValueFromSID(s)}}
}

// 0.58: a link counts as the system's only when nobody else may change,
// remove or replace it (the compatibility junctions in every profile
// belong to SYSTEM but the user has full control of them).
func TestOnlyTrustedMay(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	check := func() bool {
		p, err := windows.UTF16PtrFromString(dir)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, windows.READ_CONTROL, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			t.Fatal(err)
		}
		defer windows.CloseHandle(h) //nolint:errcheck // test handle
		return onlyTrustedMay(h, linkRights)
	}
	full := windows.ACCESS_MASK(windows.GENERIC_ALL)
	sys, admins := allow(t, windows.WinLocalSystemSid, full), allow(t, windows.WinBuiltinAdministratorsSid, full)
	read := allow(t, windows.WinWorldSid, windows.GENERIC_READ|windows.GENERIC_EXECUTE) // reading is no risk
	setDACL(t, dir, []windows.EXPLICIT_ACCESS{sys, admins, read})
	owner, err := ownerOf(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !trustedOwner(owner) {
		t.Skip("the test runs as a user who is no administrator: the folder's owner is not trusted")
	}
	if !check() {
		t.Error("only SYSTEM and Administrators may write, but the folder is not trusted")
	}
	setDACL(t, dir, []windows.EXPLICIT_ACCESS{sys, admins, read, allow(t, windows.WinAuthenticatedUserSid, windows.ACCESS_MASK(fileWriteData))})
	if check() {
		t.Error("Authenticated Users may change it, but it is trusted")
	}
	setDACL(t, dir, []windows.EXPLICIT_ACCESS{sys, admins, read, allow(t, windows.WinWorldSid, windows.DELETE)})
	if check() {
		t.Error("Everyone may delete it, but it is trusted")
	}
	setDACL(t, dir, []windows.EXPLICIT_ACCESS{sys, admins, allow(t, windows.WinWorldSid, windows.GENERIC_ALL)})
}

func ownerOf(p string) (*windows.SID, error) {
	sd, err := windows.GetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return nil, err
	}
	o, _, err := sd.Owner()
	return o, err
}

func TestReparseTargetPrefixes(t *testing.T) {
	for in, want := range map[string]string{
		`\??\C:\Users`:               `C:\Users`,
		`\??\UNC\server\share\x`:     `\\server\share\x`,
		`\??\Volume{5d07a5f1-047a}\`: `\\?\Volume{5d07a5f1-047a}\`,
	} {
		if got := ntTarget(in); got != want {
			t.Errorf("%s: %s, want %s", in, got, want)
		}
	}
	if got := linkTarget(`D:\data\x`, `\foo`); got != `D:\foo` {
		t.Errorf("root-relative target: %s", got)
	}
	if got := linkTarget(`D:\data\x`, `..\y`); got != `D:\data\y` {
		t.Errorf("relative target: %s", got)
	}
}
