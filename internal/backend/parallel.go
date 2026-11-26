package backend

// Concurrent is a backend that handles several requests at once well: an
// HTTP object store, where each upload or ranged read mostly waits on the
// network. Uploading packs and reading blocks a few at a time then
// multiplies the throughput on slow or distant providers (issue #38:
// 7 MB/s up and 2 MB/s for a restore from Hetzner Object Storage).
type Concurrent interface{ Concurrency() int }

// objectStoreConcurrency is how many packs go to an object store at once
// (16 MiB each, so it also bounds the memory held by uploads) and how many
// blocks a restore reads ahead at once.
const objectStoreConcurrency = 4

// Concurrency tells how many uploads or reads may run at once; 1 means one
// after another (local disks, SMB and SFTP keep that).
func Concurrency(be Backend) int {
	if c, ok := be.(Concurrent); ok {
		if n := c.Concurrency(); n > 1 {
			return n
		}
	}
	return 1
}

// shareReadConcurrency is how many blocks a restore reads at once from storage
// that takes uploads one after another (SMB, SFTP, local and USB disks). Those
// answer several reads at once well; with one read at a time, a disk image
// restore from an SMB share used about 40 % of a 1 Gbit/s link while the
// network and the target disk waited for each other (#65).
const shareReadConcurrency = 3

// shareUploadConcurrency is how many packs a backup uploads at once to
// storage that takes other work one after another (SMB, SFTP, local and USB
// disks). With one upload at a time, each pack waited for its flush and
// rename while the link stood still: two uploads raised a VM backup to an
// SMB share over 1 Gbit/s from 101 to 120 MB/s, and to a local disk from
// 153 to 193 MB/s (measured by gtawelt, #71).
const shareUploadConcurrency = 2

// UploadConcurrency tells how many packs a backup uploads at once: as many
// as Concurrency on object stores, shareUploadConcurrency elsewhere.
func UploadConcurrency(be Backend) int {
	return max(Concurrency(be), shareUploadConcurrency)
}

// ReadConcurrency tells how many blocks a restore may read at once: as many
// as uploads on object stores, shareReadConcurrency elsewhere.
func ReadConcurrency(be Backend) int {
	return max(Concurrency(be), shareReadConcurrency)
}

func (s *S3) Concurrency() int       { return objectStoreConcurrency }
func (a *Azure) Concurrency() int    { return objectStoreConcurrency }
func (h *Hardened) Concurrency() int { return objectStoreConcurrency }

// The throttle keeps the average upload rate with parallel uploads too (its
// limiter hands out start times one after another).
func (t *throttled) Concurrency() int { return Concurrency(t.Backend) }
