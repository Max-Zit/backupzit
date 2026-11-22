// Package leftover is the agent's ledger of what its backups create on a
// hypervisor: VM snapshots and checkpoints, temporary disk copies, the
// backup lock of a guest. An item is written before the object is created
// (or right after, where creating it may fail for a reason that is not
// ours) and removed only once the object's removal is verified, so an agent
// crash, a power loss, a cancel or a failed removal leaves a record. The
// agent's janitor works through the records (at start and every few
// minutes) and the console lists what stays (Dashboard → Attention).
package leftover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Platforms and kinds of items.
const (
	VMware  = "vmware"
	Proxmox = "proxmox"
	HyperV  = "hyperv"
	// MacOS: an APFS local snapshot a files backup of a Mac reads from
	// (KindSnapshot; VM is the volume, Detail its mount point).
	MacOS = "macos"

	KindSnapshot   = "snapshot"   // VMware, Proxmox VE or APFS snapshot
	KindCheckpoint = "checkpoint" // Hyper-V checkpoint
	KindDiskCopy   = "disk-copy"  // VMware temporary copy of a disk on a datastore
	KindLock       = "lock"       // Proxmox VE backup lock set by BackupZit
)

// Item is one object a backup created.
type Item struct {
	Platform string `json:"platform"`
	Kind     string `json:"kind"`
	// Host is the VMware connection's address; "" on the agent's own host.
	Host string `json:"host,omitempty"`
	// VM is the VMware instance UUID, the Proxmox VE guest number or the
	// Hyper-V VM ID; VMName its name when the item was made.
	VM     string `json:"vm"`
	VMName string `json:"vm_name,omitempty"`
	// Name is the snapshot or checkpoint name, the datastore path of a disk
	// copy ("[ds] dir/file.vmdk") or the lock's name.
	Name string `json:"name"`
	// Detail is platform data the clean-up needs: the guest type on Proxmox
	// VE (qemu, lxc), the completion marker on Hyper-V.
	Detail string `json:"detail,omitempty"`
	// Pending (Hyper-V): the creation was cancelled while Hyper-V still
	// worked on it, so the checkpoint may carry its default name.
	Pending bool      `json:"pending,omitempty"`
	Created time.Time `json:"created"`
	// RunID is the run that made it (0: found, not made by this agent).
	RunID int64 `json:"run_id,omitempty"`
	// Tries, Error and LastTry tell about removals that failed.
	Tries   int       `json:"tries,omitempty"`
	Error   string    `json:"error,omitempty"`
	LastTry time.Time `json:"last_try,omitempty"`
}

// Key identifies an item.
func (it Item) Key() string {
	return strings.Join([]string{it.Platform, it.Host, it.VM, it.Kind, it.Name}, "|")
}

// maxItems bounds the ledger: one stuck host must not make it grow without
// end. The oldest items go first.
const maxItems = 500

// Ledger is the file holding the items.
type Ledger struct {
	mu   sync.Mutex
	path string
}

var (
	stdMu sync.Mutex
	std   *Ledger
)

// Open makes path the ledger the package functions use (the agent service
// does this at start; without it, as in tests and command line runs, they
// do nothing).
func Open(path string) {
	stdMu.Lock()
	defer stdMu.Unlock()
	std = &Ledger{path: path}
}

// Close stops using the ledger (tests).
func Close() {
	stdMu.Lock()
	defer stdMu.Unlock()
	std = nil
}

func current() *Ledger {
	stdMu.Lock()
	defer stdMu.Unlock()
	return std
}

type runKey struct{}

// WithRun tells the package functions which run they work for.
func WithRun(ctx context.Context, id int64) context.Context {
	return context.WithValue(ctx, runKey{}, id)
}

// RunOf returns the run of ctx (0 when none).
func RunOf(ctx context.Context) int64 {
	id, _ := ctx.Value(runKey{}).(int64)
	return id
}

// Add records an item (again: a known item keeps its first time and its
// failed tries). The run comes from ctx.
func Add(ctx context.Context, it Item) {
	l := current()
	if l == nil {
		return
	}
	if it.Created.IsZero() {
		it.Created = time.Now().UTC()
	}
	if it.RunID == 0 {
		it.RunID = RunOf(ctx)
	}
	_ = l.update(func(items map[string]Item) {
		if old, ok := items[it.Key()]; ok {
			it.Created, it.Tries, it.Error, it.LastTry = old.Created, old.Tries, old.Error, old.LastTry
		}
		items[it.Key()] = it
	})
}

// Done removes an item: its object is gone.
func Done(it Item) {
	if l := current(); l != nil {
		_ = l.update(func(items map[string]Item) { delete(items, it.Key()) })
	}
}

// Failed records that removing an item failed. A nil error records a try
// without a reason.
func Failed(it Item, err error) {
	l := current()
	if l == nil {
		return
	}
	_ = l.update(func(items map[string]Item) {
		old, ok := items[it.Key()]
		if !ok {
			old = it
			if old.Created.IsZero() {
				old.Created = time.Now().UTC()
			}
		}
		old.Tries++
		old.LastTry = time.Now().UTC()
		old.Error = "removal failed"
		if err != nil {
			old.Error = clip(err.Error(), 500)
		}
		items[it.Key()] = old
	})
}

// List returns the items, oldest first.
func List() []Item {
	l := current()
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	items, _ := l.read()
	return sorted(items)
}

// ErrBusy: the VM is in use by a run of this agent; its items wait.
var ErrBusy = errors.New("the VM is in use by a backup or restore")

// Report lists the items the console shows: those whose removal failed and
// those of runs that are no longer running (active tells which are). Items
// a running run made are its own business.
func Report(active func(runID int64) bool) []Item {
	var out []Item
	for _, it := range List() {
		if it.Error == "" && it.RunID != 0 && active(it.RunID) {
			continue
		}
		out = append(out, it)
	}
	return out
}

func (l *Ledger) update(f func(map[string]Item)) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	items, err := l.read()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		// A damaged file is replaced: losing the records is better than
		// stopping to record.
		items = map[string]Item{}
	}
	f(items)
	return l.write(items)
}

func (l *Ledger) read() (map[string]Item, error) {
	items := map[string]Item{}
	b, err := os.ReadFile(l.path) // #nosec G304 -- the agent's own work file
	if err != nil {
		return items, err
	}
	var list []Item
	if err := json.Unmarshal(b, &list); err != nil {
		return map[string]Item{}, err
	}
	for _, it := range list {
		items[it.Key()] = it
	}
	return items, nil
}

func (l *Ledger) write(items map[string]Item) error {
	list := sorted(items)
	if len(list) > maxItems {
		list = list[len(list)-maxItems:]
	}
	if len(list) == 0 {
		if err := os.Remove(l.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, err := json.MarshalIndent(list, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return err
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, l.path)
}

func sorted(items map[string]Item) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		out = append(out, it)
	}
	sort.Slice(out, func(i, k int) bool {
		if !out[i].Created.Equal(out[k].Created) {
			return out[i].Created.Before(out[k].Created)
		}
		return out[i].Key() < out[k].Key()
	})
	return out
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Retry runs remove until verify reports the object gone, up to the given
// waits between tries (the removal of a large snapshot can need several
// minutes, or a host can be busy for a while). It returns nil when the
// object is gone, else the last error. ctx should not be the run's own
// context when the run may be cancelled: the clean-up still has to happen.
func Retry(ctx context.Context, waits []time.Duration, remove func(context.Context) error, gone func(context.Context) (bool, error)) error {
	var last error
	for i := 0; ; i++ {
		err := remove(ctx)
		ok, verr := gone(ctx)
		switch {
		case verr == nil && ok:
			return nil
		case err != nil:
			last = err
		case verr != nil:
			last = verr
		default:
			last = errors.New("it is still there after the removal")
		}
		if i >= len(waits) {
			return last
		}
		select {
		case <-ctx.Done():
			return errors.Join(last, ctx.Err())
		case <-time.After(waits[i]):
		}
	}
}

// RunWaits are the waits between removal tries inside a run: three tries
// over about ten minutes, then the janitor takes over.
var RunWaits = []time.Duration{2 * time.Minute, 6 * time.Minute}

// CancelError is the error of a cancelled backup that keeps what its
// clean-up reported (a snapshot that could not be removed); errors.Is still
// finds the cancel.
func CancelError(err error, warns []string) error {
	if len(warns) == 0 {
		return err
	}
	return fmt.Errorf("%w; %s", err, strings.Join(warns, "; "))
}
