package repo

import "errors"

// ValidateVMBlockSizeKiB accepts the three supported VM disk block sizes.
// Zero is the historical default, so existing jobs and agents keep 1 MiB.
func ValidateVMBlockSizeKiB(kib int) error {
	switch kib {
	case 0, 256, 512, 1024:
		return nil
	default:
		return errors.New("VM block size must be 256, 512 or 1024 KiB")
	}
}

// SetVMBlockSizeKiB selects the block size for new virtual disk images in
// this session only. Each image stores its own block size; opening or
// restoring existing backups never depends on this setting.
func (r *Repository) SetVMBlockSizeKiB(kib int) error {
	if err := ValidateVMBlockSizeKiB(kib); err != nil {
		return err
	}
	r.mu.Lock()
	r.vmBlockSizeKiB = kib
	r.mu.Unlock()
	return nil
}

func (r *Repository) VMBlockSizeKiB() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.vmBlockSizeKiB == 0 {
		return 1024
	}
	return r.vmBlockSizeKiB
}
