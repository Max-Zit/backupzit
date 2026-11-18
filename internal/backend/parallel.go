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

func (s *S3) Concurrency() int       { return objectStoreConcurrency }
func (a *Azure) Concurrency() int    { return objectStoreConcurrency }
func (h *Hardened) Concurrency() int { return objectStoreConcurrency }

// The throttle keeps the average upload rate with parallel uploads too (its
// limiter hands out start times one after another).
func (t *throttled) Concurrency() int { return Concurrency(t.Backend) }
