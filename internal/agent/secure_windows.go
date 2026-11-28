package agent

import (
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// protectDir replaces the inherited ACL of dir (and everything in it) with
// one that grants access only to SYSTEM and Administrators. ProgramData is
// readable by all users by default, and the agent configuration holds the
// agent secret, with which storage credentials can be obtained.
//
// It also makes SYSTEM (or, from an administrator's command line, which may
// not set SYSTEM, the Administrators group) the owner of dir and of
// everything in it: a local user who created the folder before the agent
// was installed would otherwise stay its owner, keep the implicit right to
// change its ACL and grant themselves access again (to agent.json, to the
// scripts run as SYSTEM, to an update between its check and msiexec;
// M-N10).
func protectDir(dir string) error {
	// On a junction or symbolic link the access list would go to the link
	// object only, and the folder it points to stays open (HIGH 6): such a
	// folder (the update or commands folder) is refused.
	if link, err := reparsePoint(dir); err != nil {
		return err
	} else if link {
		return fmt.Errorf("%s is a link (junction or symbolic link), not a folder of the agent; remove it", dir)
	}
	sd, err := windows.SecurityDescriptorFromString(configDirSDDL)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	const withOwner = windows.OWNER_SECURITY_INFORMATION | windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	const aclOnly = windows.DACL_SECURITY_INFORMATION | windows.PROTECTED_DACL_SECURITY_INFORMATION
	set := func(p string) error {
		var err error
		for _, sid := range []windows.WELL_KNOWN_SID_TYPE{windows.WinLocalSystemSid, windows.WinBuiltinAdministratorsSid} {
			owner, serr := windows.CreateWellKnownSid(sid)
			if serr != nil {
				continue
			}
			if err = windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, withOwner, owner, nil, dacl, nil); err == nil {
				return nil
			}
		}
		// Not allowed to change the owner: at least the ACL, as before.
		if aerr := windows.SetNamedSecurityInfo(p, windows.SE_FILE_OBJECT, aclOnly, nil, nil, dacl, nil); aerr != nil {
			return aerr
		}
		return nil
	}
	if err := set(dir); err != nil {
		return err
	}
	// The files and folders inside (a planted script, an old download) get
	// the same owner and lose any ACL of their own.
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || d.Type()&fs.ModeIrregular != 0 {
			return nil // a link or junction is not followed into another tree
		}
		_ = set(p)
		return nil
	})
}
