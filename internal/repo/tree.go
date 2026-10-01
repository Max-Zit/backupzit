package repo

import (
	"sort"
	"time"
)

// Node types.
const (
	NodeFile    = "file"
	NodeDir     = "dir"
	NodeSymlink = "symlink"
)

// Node is one entry of a directory.
type Node struct {
	Name    string    `json:"name"`
	Type    string    `json:"type"`
	Mode    uint32    `json:"mode"` // fs.FileMode bits
	ModTime time.Time `json:"mtime"`
	Size    uint64    `json:"size,omitempty"`
	// Content lists the data blobs of a file in order.
	Content []ID `json:"content,omitempty"`
	// Subtree is the tree blob of a directory.
	Subtree *ID `json:"subtree,omitempty"`
	// LinkTarget is the target of a symlink.
	LinkTarget string `json:"link_target,omitempty"`
	// WinAttrs holds Windows file attribute flags (FILE_ATTRIBUTE_*).
	WinAttrs uint32 `json:"win_attrs,omitempty"`
}

// Tree is the content of a directory, sorted by name.
type Tree struct {
	Nodes []Node `json:"nodes"`
}

// Sort orders nodes by name.
func (t *Tree) Sort() {
	sort.Slice(t.Nodes, func(i, j int) bool { return t.Nodes[i].Name < t.Nodes[j].Name })
}

// Find returns the node with the given name, or nil.
func (t *Tree) Find(name string) *Node {
	i := sort.Search(len(t.Nodes), func(i int) bool { return t.Nodes[i].Name >= name })
	if i < len(t.Nodes) && t.Nodes[i].Name == name {
		return &t.Nodes[i]
	}
	return nil
}

// Snapshot records one backup run.
type Snapshot struct {
	ID       ID        `json:"-"`
	Time     time.Time `json:"time"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username,omitempty"`
	// Paths are the source paths as given by the user (absolute).
	Paths []string `json:"paths"`
	Tags  []string `json:"tags,omitempty"`
	// Tree is the root tree. Source paths are stored under their full path,
	// with the volume as first component ("C" for C:\, "" root for Unix).
	Tree   ID            `json:"tree"`
	Parent *ID           `json:"parent,omitempty"`
	Stats  SnapshotStats `json:"stats"`
	// ProgramVersion is the agent version that created the snapshot.
	ProgramVersion string `json:"program_version"`
	// VSSVolumes lists volumes read from a Volume Shadow Copy snapshot.
	VSSVolumes []string `json:"vss_volumes,omitempty"`
	// Images is set for disk image backups (the file tree is then empty).
	Images []DiskImage `json:"images,omitempty"`
}

// SnapshotStats summarises a backup run.
type SnapshotStats struct {
	Files        uint64        `json:"files"`
	Dirs         uint64        `json:"dirs"`
	Bytes        uint64        `json:"bytes"`         // total size of files in the snapshot
	FilesNew     uint64        `json:"files_new"`     // files not in parent
	FilesChanged uint64        `json:"files_changed"` // files read again
	FilesSkipped uint64        `json:"files_unchanged"`
	BytesRead    uint64        `json:"bytes_read"`
	BytesAdded   uint64        `json:"bytes_added"`  // new unique data, uncompressed
	BytesStored  uint64        `json:"bytes_stored"` // new unique data, as uploaded
	Duration     time.Duration `json:"duration_ns"`
	Errors       []string      `json:"errors,omitempty"`
}
