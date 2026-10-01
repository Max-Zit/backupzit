package hardened

import (
	"os"

	"golang.org/x/sys/unix"
)

const fsImmutableFL = 0x00000010 // FS_IMMUTABLE_FL, "chattr +i"

// setImmutable sets or clears the filesystem immutable attribute. While it
// is set nobody, not even root, can modify, rename or delete the file
// without clearing it first, which needs CAP_LINUX_IMMUTABLE.
func setImmutable(path string, on bool) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	flags, err := unix.IoctlGetUint32(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err != nil {
		return &os.PathError{Op: "get attributes", Path: path, Err: err}
	}
	nf := flags &^ fsImmutableFL
	if on {
		nf = flags | fsImmutableFL
	}
	if nf == flags {
		return nil
	}
	if err := unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, int(nf)); err != nil {
		return &os.PathError{Op: "set immutable attribute", Path: path, Err: err}
	}
	return nil
}

func immutableSupported() bool { return true }
