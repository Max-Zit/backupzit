package agent

import (
	"golang.org/x/sys/windows"
)

// protectDir replaces the inherited ACL of dir (and everything in it) with
// one that grants access only to SYSTEM and Administrators. ProgramData is
// readable by all users by default, and the agent configuration holds the
// agent secret, with which storage credentials can be obtained.
func protectDir(dir string) error {
	sd, err := windows.SecurityDescriptorFromString("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(dir, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}
