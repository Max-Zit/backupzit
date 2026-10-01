// Command backupzit-agent backs up and restores files and folders.
//
// Phase 1 is a command line tool; later it runs as a service controlled by
// the backupzit management console.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/backupzit/backupzit/internal/archiver"
	"github.com/backupzit/backupzit/internal/backend"
	"github.com/backupzit/backupzit/internal/checker"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/restorer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `backupzit-agent ` + "%s" + `

Usage:
  backupzit-agent <command> [options]

Managed mode (controlled by the backupzit server):
  enroll      Register this machine with the management server
  service     install | uninstall | start | stop | status | run

Standalone commands:
  init        Create a new repository
  backup      Back up files and folders
  snapshots   List snapshots
  ls          List files in a snapshot
  restore     Restore files from a snapshot
  check       Verify repository integrity
  version     Print version

Repository options (all commands):
  --repo         Repository location: a local path or sftp://user@host[:port]/path
                 (env BACKUPZIT_REPO)
  --sftp-password  SFTP password (env BACKUPZIT_SFTP_PASSWORD)
  --sftp-key       Path to an SSH private key
  --sftp-hostkey   Expected server host key fingerprint "SHA256:..." (env BACKUPZIT_SFTP_HOSTKEY)

Run "backupzit-agent <command> -h" for command options.
`

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

type repoFlags struct {
	location string
	opts     backend.Options
}

func addRepoFlags(fs *flag.FlagSet) *repoFlags {
	rf := &repoFlags{}
	fs.StringVar(&rf.location, "repo", os.Getenv("BACKUPZIT_REPO"), "repository location")
	fs.StringVar(&rf.opts.SFTPPassword, "sftp-password", os.Getenv("BACKUPZIT_SFTP_PASSWORD"), "SFTP password")
	fs.StringVar(&rf.opts.SFTPKeyFile, "sftp-key", "", "SSH private key file")
	fs.StringVar(&rf.opts.SFTPHostKey, "sftp-hostkey", os.Getenv("BACKUPZIT_SFTP_HOSTKEY"), "expected host key fingerprint")
	fs.BoolVar(&rf.opts.SFTPInsecure, "sftp-insecure", false, "do not verify the SFTP host key (testing only)")
	return rf
}

func (rf *repoFlags) backend(ctx context.Context) (backend.Backend, error) {
	if rf.location == "" {
		return nil, errors.New("no repository given (use --repo or BACKUPZIT_REPO)")
	}
	return backend.Open(ctx, rf.location, rf.opts)
}

func (rf *repoFlags) open(ctx context.Context) (*repo.Repository, error) {
	be, err := rf.backend(ctx)
	if err != nil {
		return nil, err
	}
	r, err := repo.Open(ctx, be)
	if err != nil {
		be.Close()
		return nil, err
	}
	return r, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Printf(usage, version)
		return nil
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "init":
		return cmdInit(ctx, rest)
	case "backup":
		return cmdBackup(ctx, rest)
	case "snapshots":
		return cmdSnapshots(ctx, rest)
	case "ls":
		return cmdLs(ctx, rest)
	case "restore":
		return cmdRestore(ctx, rest)
	case "check":
		return cmdCheck(ctx, rest)
	case "image-backup":
		return cmdImageBackup(ctx, rest)
	case "image-restore":
		return cmdImageRestore(ctx, rest)
	case "image-ls":
		return cmdImageLs(ctx, rest)
	case "image-extract":
		return cmdImageExtract(ctx, rest)
	case "recovery":
		return cmdRecovery(ctx, rest)
	case "disks":
		return cmdDisks(rest)
	case "enroll":
		return cmdEnroll(ctx, rest)
	case "service":
		return cmdService(rest)
	case "version":
		fmt.Println("backupzit-agent", version)
		return nil
	}
	return fmt.Errorf("unknown command %q (see backupzit-agent help)", cmd)
}

func cmdInit(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	rf := addRepoFlags(fs)
	fs.Parse(args)
	be, err := rf.backend(ctx)
	if err != nil {
		return err
	}
	r, err := repo.Init(ctx, be)
	if err != nil {
		be.Close()
		return err
	}
	defer r.Close()
	fmt.Printf("created repository %s at %s\n", r.Config().ID, be.Location())
	return nil
}

func cmdBackup(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("backup", flag.ExitOnError)
	rf := addRepoFlags(fs)
	var tags, excludes multiFlag
	fs.Var(&tags, "tag", "tag for the snapshot (repeatable)")
	fs.Var(&excludes, "exclude", "glob pattern of names to skip (repeatable)")
	noParent := fs.Bool("force", false, "read all files even if unchanged since the last snapshot")
	useVSS := fs.Bool("vss", false, "read from a Volume Shadow Copy snapshot (Windows, requires administrator)")
	host := fs.String("host", "", "override hostname stored in the snapshot")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent backup [options] <path>...")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() == 0 {
		fs.Usage()
		return errors.New("no paths given")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()

	last := time.Now()
	sn, err := archiver.Run(ctx, r, archiver.Options{
		Paths:    fs.Args(),
		Tags:     tags,
		Excludes: excludes,
		NoParent: *noParent,
		VSS:      *useVSS,
		Hostname: *host,
		Version:  version,
		Progress: func(p string, s *repo.SnapshotStats) {
			if time.Since(last) > 2*time.Second {
				last = time.Now()
				fmt.Printf("  %d files, %s read ... %s\n", s.Files, humanBytes(s.BytesRead), p)
			}
		},
	})
	if err != nil {
		return err
	}
	st := sn.Stats
	fmt.Printf("snapshot %s saved\n", sn.ID.Short())
	if len(sn.VSSVolumes) > 0 {
		fmt.Printf("  vss:    read from snapshot of %s\n", strings.Join(sn.VSSVolumes, ", "))
	}
	fmt.Printf("  files:  %d new, %d changed, %d unchanged (%d dirs)\n", st.FilesNew, st.FilesChanged, st.FilesSkipped, st.Dirs)
	fmt.Printf("  data:   %s total, %s read, %s new (%s stored)\n",
		humanBytes(st.Bytes), humanBytes(st.BytesRead), humanBytes(st.BytesAdded), humanBytes(st.BytesStored))
	fmt.Printf("  time:   %s\n", st.Duration.Round(time.Millisecond))
	if len(st.Errors) > 0 {
		fmt.Printf("  %d errors:\n", len(st.Errors))
		for _, e := range st.Errors {
			fmt.Println("   ", e)
		}
		return fmt.Errorf("backup finished with %d errors", len(st.Errors))
	}
	return nil
}

func cmdSnapshots(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("snapshots", flag.ExitOnError)
	rf := addRepoFlags(fs)
	fs.Parse(args)
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sns, err := r.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tTime\tHost\tFiles\tSize\tPaths")
	for _, sn := range sns {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%s\t%s\n", sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04:05"),
			sn.Hostname, sn.Stats.Files, humanBytes(sn.Stats.Bytes), strings.Join(sn.Paths, ", "))
	}
	tw.Flush()
	fmt.Printf("%d snapshots\n", len(sns))
	return nil
}

func cmdLs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ls", flag.ExitOnError)
	rf := addRepoFlags(fs)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent ls [options] <snapshot-id|latest>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("snapshot id required")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	var walk func(id repo.ID, prefix string) error
	walk = func(id repo.ID, prefix string) error {
		t, err := r.LoadTree(ctx, id)
		if err != nil {
			return err
		}
		for _, n := range t.Nodes {
			p := prefix + "/" + n.Name
			switch n.Type {
			case repo.NodeDir:
				fmt.Printf("d %12s  %s  %s/\n", "", n.ModTime.Local().Format("2006-01-02 15:04"), p)
				if n.Subtree != nil {
					if err := walk(*n.Subtree, p); err != nil {
						return err
					}
				}
			case repo.NodeFile:
				fmt.Printf("f %12d  %s  %s\n", n.Size, n.ModTime.Local().Format("2006-01-02 15:04"), p)
			case repo.NodeSymlink:
				fmt.Printf("l %12s  %s  %s -> %s\n", "", n.ModTime.Local().Format("2006-01-02 15:04"), p, n.LinkTarget)
			}
		}
		return nil
	}
	return walk(sn.Tree, "")
}

func cmdRestore(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	rf := addRepoFlags(fs)
	target := fs.String("target", "", "directory to restore into (required unless --original)")
	original := fs.Bool("original", false, "restore files to their original locations (overwrites existing files)")
	verify := fs.Bool("verify", false, "re-read restored files and compare them with the snapshot")
	var includes multiFlag
	fs.Var(&includes, "include", "restore only this path from the snapshot (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "Usage: backupzit-agent restore [options] <snapshot-id|latest>")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return errors.New("snapshot id required")
	}
	if (*target == "") == !*original {
		return errors.New("specify exactly one of --target or --original")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	fmt.Printf("restoring snapshot %s from %s\n", sn.ID.Short(), sn.Time.Local().Format("2006-01-02 15:04:05"))
	st, err := restorer.Run(ctx, r, sn, restorer.Options{Target: *target, Include: includes, Verify: *verify})
	if err != nil {
		return err
	}
	fmt.Printf("restored %d files, %d dirs, %d symlinks, %s in %s\n",
		st.Files, st.Dirs, st.Symlinks, humanBytes(st.Bytes), st.Duration.Round(time.Millisecond))
	if *verify {
		fmt.Printf("verified %d files\n", st.FilesVerified)
	}
	if len(st.Errors) > 0 {
		for _, e := range st.Errors {
			fmt.Println("  ", e)
		}
		return fmt.Errorf("restore finished with %d errors", len(st.Errors))
	}
	return nil
}

func cmdCheck(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("check", flag.ExitOnError)
	rf := addRepoFlags(fs)
	readData := fs.Bool("read-data", false, "download and verify all data (slow)")
	fs.Parse(args)
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	res, err := checker.Run(ctx, r, checker.Options{ReadData: *readData})
	if err != nil {
		return err
	}
	fmt.Printf("%d snapshots, %d trees, %d data blobs, %d packs\n", res.Snapshots, res.Trees, res.DataBlobs, res.Packs)
	if *readData {
		fmt.Printf("read %d packs, verified %d blobs\n", res.PacksRead, res.BlobsRead)
	}
	for _, w := range res.Warnings {
		fmt.Println("warning:", w)
	}
	for _, e := range res.Errors {
		fmt.Println("ERROR:", e)
	}
	if !res.OK() {
		return fmt.Errorf("check found %d errors", len(res.Errors))
	}
	fmt.Println("no errors found")
	return nil
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
