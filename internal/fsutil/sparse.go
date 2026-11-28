package fsutil

// Hole is a range of zeros a restore skipped (a seek) instead of writing
// it, so the restored file stays sparse: offset and length in bytes. The
// file-backup restore and the guest file restore share MarkSparse and
// PunchHoles for them.
type Hole struct{ Off, N int64 }
