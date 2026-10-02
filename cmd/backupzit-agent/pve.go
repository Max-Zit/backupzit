package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/backupzit/backupzit/internal/pve"
	"github.com/backupzit/backupzit/internal/repo"
	"github.com/backupzit/backupzit/internal/vmfs"
)

// cmdPVE handles: pve list|backup|restore|show (agent installed on a
// Proxmox VE node; backs up VMs and containers without agents in them).
func cmdPVE(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent pve list|backup|show|ls|extract|restore [options]")
	}
	if args[0] != "show" && args[0] != "ls" && args[0] != "extract" && !pve.Available() {
		return errors.New("this machine is not a Proxmox VE node (no /etc/pve/storage.cfg or qm)")
	}
	switch args[0] {
	case "list":
		inv, err := pve.GetInventory(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("node %s  %s\n", inv.Node, inv.Version)
		fmt.Printf("%-8s %-5s %-24s %-10s %-10s %s\n", "VMID", "TYPE", "NAME", "NODE", "STATUS", "DISK")
		for _, g := range inv.Guests {
			fmt.Printf("%-8d %-5s %-24s %-10s %-10s %s\n", g.VMID, g.Type, g.Name, g.Node, g.Status, humanBytes(g.MaxDisk))
		}
		fmt.Println("storage for restores:", strings.Join(inv.Storage, ", "))
		return nil
	case "backup":
		fs := flag.NewFlagSet("pve backup", flag.ExitOnError)
		rf := addRepoFlags(fs)
		ids := fs.String("vmid", "", "comma-separated guest IDs (default: all guests on this node)")
		excl := fs.String("exclude", "", "comma-separated guest IDs to skip when backing up all guests")
		fs.Parse(args[1:])
		var sel []int
		if *ids != "" {
			s, err := parseInts(*ids)
			if err != nil {
				return err
			}
			sel = s
		}
		ex, err := parseInts(*excl)
		if err != nil {
			return err
		}
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		lock, err := r.Lock(ctx, false, time.Minute)
		if err != nil {
			return err
		}
		defer lock.Unlock()
		sn, err := pve.Backup(ctx, r, pve.BackupOptions{VMIDs: sel, Exclude: ex, Version: version, Progress: progressPrinter("read"),
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		if _, err := r.KeepImmutable(ctx, sn); err != nil {
			return fmt.Errorf("extend immutability: %w", err)
		}
		fmt.Printf("snapshot %s saved\n", sn.ID.Short())
		printGuests(sn.Guests, sn.Images)
		st := sn.Stats
		fmt.Printf("  data: %s read, %s new (%s stored), %s\n", humanBytes(st.BytesRead), humanBytes(st.BytesAdded), humanBytes(st.BytesStored), st.Duration.Round(time.Second))
		for _, e := range st.Errors {
			fmt.Println("  warning:", e)
		}
		if len(st.Errors) > 0 {
			return fmt.Errorf("backup finished with %d warnings", len(st.Errors))
		}
		return nil
	case "show":
		fs := flag.NewFlagSet("pve show", flag.ExitOnError)
		rf := addRepoFlags(fs)
		fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return errors.New("usage: backupzit-agent pve show [options] <snapshot-id|latest>")
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
		if len(sn.Guests) == 0 {
			return fmt.Errorf("snapshot %s is not a Proxmox backup", sn.ID.Short())
		}
		fmt.Printf("snapshot %s from node %s, %s\n", sn.ID.Short(), sn.Hostname, sn.Time.Local().Format("2006-01-02 15:04:05"))
		printGuests(sn.Guests, sn.Images)
		return nil
	case "ls", "extract":
		return cmdPVEFiles(ctx, args[0], args[1:])
	case "restore":
		fs := flag.NewFlagSet("pve restore", flag.ExitOnError)
		rf := addRepoFlags(fs)
		vmid := fs.Int("vmid", 0, "guest to restore from the snapshot")
		newID := fs.Int("new-vmid", 0, "ID for the restored guest (default: original ID; -1 = next free ID)")
		name := fs.String("name", "", "name for the restored guest (default: original name)")
		storage := fs.String("storage", "", "put all disks on this storage (default: original storage)")
		overwrite := fs.Bool("overwrite", false, "replace an existing guest with the same ID (it is stopped and deleted)")
		start := fs.Bool("start", false, "start the guest after the restore")
		fs.Usage = func() {
			fmt.Fprintln(os.Stderr, "Usage: backupzit-agent pve restore [options] --vmid N <snapshot-id|latest>")
			fs.PrintDefaults()
		}
		fs.Parse(args[1:])
		if fs.NArg() != 1 || *vmid == 0 {
			fs.Usage()
			return errors.New("snapshot and --vmid are required")
		}
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		lock, err := r.Lock(ctx, false, time.Minute)
		if err != nil {
			return err
		}
		defer lock.Unlock()
		sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := pve.Restore(ctx, r, sn, pve.RestoreOptions{VMID: *vmid, NewVMID: *newID, Name: *name, Storage: *storage,
			Overwrite: *overwrite, Start: *start, Progress: progressPrinter("written"),
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		fmt.Printf("restored %s %d (%s) on node %s\n", res.Type, res.VMID, res.Name, res.Node)
		for _, d := range res.Disks {
			fmt.Println("  disk", d)
		}
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		if res.Started {
			fmt.Println("  started")
		}
		return nil
	}
	return fmt.Errorf("unknown pve command %q", args[0])
}

func printGuests(gs []repo.Guest, imgs []repo.DiskImage) {
	for _, g := range gs {
		fmt.Printf("  %s %d %q: %s\n", g.Type, g.VMID, g.Name, g.Consistency)
		for _, d := range g.Disks {
			stored := uint64(0)
			if d.Image < len(imgs) && len(imgs[d.Image].Partitions) == 1 {
				stored = imgs[d.Image].Partitions[0].StoredBytes
			}
			fmt.Printf("    %-10s %-28s %8s, %s with data\n", d.Key, d.Volume, humanBytes(d.Size), humanBytes(stored))
		}
	}
}

// cmdPVEFiles lists or extracts files inside a VM disk of a Proxmox backup,
// without restoring the VM (works on any machine with access to the
// repository).
func cmdPVEFiles(ctx context.Context, cmd string, args []string) error {
	fs := flag.NewFlagSet("pve "+cmd, flag.ExitOnError)
	rf := addRepoFlags(fs)
	vmid := fs.Int("vmid", 0, "guest in the backup")
	disk := fs.String("disk", "", "disk of the guest, e.g. scsi0 (default: first disk)")
	volume := fs.String("volume", "", "partition or logical volume, e.g. p1 or lvm/vg/root (default: first one with files)")
	target := fs.String("target", "", "extract: folder to copy the files into")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: backupzit-agent pve %s [options] --vmid N <snapshot-id|latest> [path ...]\n", cmd)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() < 1 || *vmid == 0 || (cmd == "extract" && (*target == "" || fs.NArg() < 2)) {
		fs.Usage()
		return errors.New("snapshot and --vmid are required (extract: also --target and paths)")
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
	rd, size, d, err := vmfs.GuestDisk(ctx, r, sn, *vmid, *disk)
	if err != nil {
		return err
	}
	vols, err := vmfs.Volumes(rd, size)
	if err != nil {
		return err
	}
	if cmd == "ls" && fs.NArg() == 1 && *volume == "" {
		fmt.Printf("guest %d disk %s (%s):\n", *vmid, d.Key, humanBytes(uint64(size)))
		for _, v := range vols {
			fmt.Printf("  %-22s %10s  %-5s %s %s\n", v.ID, humanBytes(uint64(v.Size)), v.FS, v.Name, v.Label)
		}
	}
	v, err := vmfs.FindVolume(vols, *volume)
	if err != nil {
		return err
	}
	fsys, err := vmfs.Open(v)
	if err != nil {
		return err
	}
	if cmd == "extract" {
		st, err := vmfs.Extract(ctx, fsys, fs.Args()[1:], *target)
		if err != nil {
			return err
		}
		fmt.Printf("extracted %d files and %d folders (%s) from %s into %s\n", st.Files, st.Dirs, humanBytes(st.Bytes), v.Name, *target)
		for _, e := range st.Errors {
			fmt.Println("  error:", e)
		}
		return nil
	}
	dir := "/"
	if fs.NArg() > 1 {
		dir = fs.Arg(1)
	}
	entries, err := fsys.List(dir)
	if err != nil {
		return err
	}
	fmt.Printf("%s %s:\n", v.Name, vmfs.Clean(dir))
	for _, e := range entries {
		kind := "     "
		if e.IsDir {
			kind = "<DIR>"
		}
		fmt.Printf("  %s %10s  %s  %s\n", kind, humanBytes(uint64(e.Size)), e.ModTime.Local().Format("2006-01-02 15:04"), e.Name)
	}
	return nil
}
