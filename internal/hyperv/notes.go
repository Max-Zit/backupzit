package hyperv

import "strings"

// onCSV reports whether a folder is on a Cluster Shared Volume (they are
// mounted under C:\ClusterStorage on every node).
func onCSV(dir string) bool {
	return strings.Contains(strings.ToLower(strings.ReplaceAll(dir, "/", `\`))+`\`, `\clusterstorage\`)
}

// tpmNote turns the TPM: line of the restore script into a note.
func tpmNote(line string) string {
	if line == "TPM:kept" {
		return "vTPM restored from the backup: BitLocker and other TPM keys in the guest keep working"
	}
	why := strings.TrimSpace(strings.TrimPrefix(line, "TPM:new:"))
	if !strings.HasPrefix(why, "the backup has no vTPM state") {
		why = "this host cannot unlock the backup's vTPM: it does not have the guardian key of the original host"
	}
	return "The VM got a new, empty vTPM (" + why + "); BitLocker in the guest asks for its recovery key. To keep the vTPM, restore on the original host, or import that host's guardian certificate (UntrustedGuardian) here first"
}
