package agent

import "golang.org/x/sys/windows/registry"

// MarkEnrolled records the enrollment where the installer can see it
// (the configuration folder itself is readable only by administrators),
// so upgrades do not ask for an enrollment code again.
func MarkEnrolled() {
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, `SOFTWARE\BackupZit\Agent`, registry.SET_VALUE)
	if err != nil {
		return
	}
	defer k.Close()
	k.SetDWordValue("Enrolled", 1)
}
