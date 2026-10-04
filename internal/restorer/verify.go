package restorer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path"
	"time"

	"github.com/max-zit/backupzit/internal/repo"
)

// SampleOptions configure a restore test.
type SampleOptions struct {
	Files    int    // number of random files to restore (file backups)
	MaxBytes uint64 // upper limit for their total size
	Blocks   int    // number of random blocks to read (disk images)
	TempDir  string // where files are restored; removed afterwards
}

// SampleStats is the result of a restore test.
type SampleStats struct {
	Files      int           `json:"files"`
	Bytes      uint64        `json:"bytes"`
	Blocks     int           `json:"blocks"`
	Candidates int           `json:"candidates"`
	Duration   time.Duration `json:"duration_ns"`
	Errors     []string      `json:"errors,omitempty"`
}

// SampleVerify proves that a snapshot can really be restored: it restores
// a random sample of its files into a temporary folder and verifies every
// byte against the stored hashes, or for disk images reads a random sample
// of blocks. All metadata (trees, block maps) on the way is read and
// verified too.
func SampleVerify(ctx context.Context, r *repo.Repository, sn *repo.Snapshot, o SampleOptions) (*SampleStats, error) {
	start := time.Now()
	st := &SampleStats{}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	if len(sn.Images) > 0 {
		var blocks []repo.ID
		for _, img := range sn.Images {
			for i := range img.Partitions {
				p := &img.Partitions[i]
				if !p.Included {
					continue
				}
				ids, err := r.LoadBlockMap(ctx, p)
				if err != nil {
					return st, fmt.Errorf("partition %d block map: %w", p.Number, err)
				}
				for _, id := range ids {
					if !id.IsNull() {
						blocks = append(blocks, id)
					}
				}
			}
		}
		st.Candidates = len(blocks)
		rng.Shuffle(len(blocks), func(i, j int) { blocks[i], blocks[j] = blocks[j], blocks[i] })
		for _, id := range blocks[:min(o.Blocks, len(blocks))] {
			data, err := r.LoadBlob(ctx, repo.DataBlob, id)
			if err != nil {
				st.Errors = append(st.Errors, fmt.Sprintf("block %s: %v", id.Short(), err))
				continue
			}
			st.Blocks++
			st.Bytes += uint64(len(data))
		}
		st.Duration = time.Since(start)
		return st, nil
	}

	type file struct {
		path string
		size uint64
	}
	var files []file
	var walk func(id repo.ID, dir string) error
	walk = func(id repo.ID, dir string) error {
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			return err
		}
		for _, n := range t.Nodes {
			p := path.Join(dir, n.Name)
			switch {
			case n.Subtree != nil:
				if err := walk(*n.Subtree, p); err != nil {
					return err
				}
			case n.Type == repo.NodeFile && len(files) < 200000:
				files = append(files, file{p, n.Size})
			}
		}
		return nil
	}
	if err := walk(sn.Tree, ""); err != nil {
		return st, fmt.Errorf("read file list: %w", err)
	}
	st.Candidates = len(files)
	rng.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	var include []string
	var total uint64
	for _, f := range files {
		if len(include) >= o.Files {
			break
		}
		if total+f.size > o.MaxBytes && len(include) > 0 {
			continue
		}
		include = append(include, f.path)
		total += f.size
	}
	if len(include) == 0 {
		st.Duration = time.Since(start)
		return st, nil
	}
	dir, err := os.MkdirTemp(o.TempDir, "backupzit-restore-test-")
	if err != nil {
		return st, err
	}
	defer os.RemoveAll(dir)
	rs, err := Run(ctx, r, sn, Options{Target: dir, Include: include, Verify: true})
	if err != nil {
		return st, err
	}
	st.Files = int(rs.FilesVerified)
	st.Bytes = rs.Bytes
	st.Errors = rs.Errors
	if int(rs.FilesVerified) < len(include) && len(rs.Errors) == 0 {
		st.Errors = append(st.Errors, fmt.Sprintf("only %d of %d files could be verified", rs.FilesVerified, len(include)))
	}
	st.Duration = time.Since(start)
	return st, nil
}
