package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/hyperv"
)

// cmdHyperV handles: hyperv list|backup|restore (agent on a Hyper-V host).
func cmdHyperV(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent hyperv list|backup|restore [options]")
	}
	if !hyperv.Available() {
		return errors.New("this machine is not a Hyper-V host (vmms service not found)")
	}
	switch args[0] {
	case "rct":
		fs := flag.NewFlagSet("hyperv rct", flag.ExitOnError)
		disk := fs.String("disk", "", "VHDX file")
		since := fs.String("since", "", "change tracking ID to compare with")
		fs.Parse(args[1:])
		on, id, changed, n, err := hyperv.RCTInfo(*disk, *since)
		if err != nil {
			return err
		}
		fmt.Printf("change tracking: %v\nmost recent ID: %s\n", on, id)
		if *since != "" {
			fmt.Printf("changed since %s: %s in %d ranges\n", *since, humanBytes(changed), n)
		}
		return nil
	case "list":
		inv, err := hyperv.GetInventory(ctx)
		if err != nil {
			return err
		}
		fmt.Printf("%s  %s\n", inv.Node, inv.Version)
		fmt.Printf("%-11s %-28s %-10s %10s  %s\n", "VMID", "NAME", "STATE", "DISKS", "HYPER-V ID")
		for _, g := range inv.Guests {
			fmt.Printf("%-11d %-28s %-10s %10s  %s\n", g.VMID, g.Name, g.Status, humanBytes(g.MaxDisk), g.ID)
		}
		fmt.Println("folders for restored disks:", strings.Join(inv.Storage, ", "))
		return nil
	case "backup":
		fs := flag.NewFlagSet("hyperv backup", flag.ExitOnError)
		rf := addRepoFlags(fs)
		ids := fs.String("vmid", "", "comma-separated VMIDs from 'hyperv list' (default: all VMs)")
		fs.Parse(args[1:])
		var sel []int
		if *ids != "" {
			s, err := parseInts(*ids)
			if err != nil {
				return err
			}
			sel = s
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
		sn, err := hyperv.Backup(ctx, r, hyperv.BackupOptions{VMIDs: sel, Version: version, Progress: progressPrinter("read"),
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
		return nil
	case "restore":
		fs := flag.NewFlagSet("hyperv restore", flag.ExitOnError)
		rf := addRepoFlags(fs)
		vmid := fs.Int("vmid", 0, "VM in the backup (VMID from 'pve show' / 'hyperv list')")
		asNew := fs.Bool("new", false, "create a second VM next to the original (new name and MAC addresses)")
		name := fs.String("name", "", "name of the restored VM")
		folder := fs.String("folder", "", "folder for the virtual disks (default: the host's default)")
		overwrite := fs.Bool("overwrite", false, "replace the original VM if it still exists (its disk files are renamed, not deleted)")
		start := fs.Bool("start", false, "start the VM after the restore")
		fs.Parse(args[1:])
		if fs.NArg() != 1 || *vmid == 0 {
			return errors.New("usage: backupzit-agent hyperv restore [options] --vmid N <snapshot-id|latest>")
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
		res, err := hyperv.Restore(ctx, r, sn, hyperv.RestoreOptions{VMID: *vmid, AsNew: *asNew, Name: *name, Folder: *folder,
			Overwrite: *overwrite, Start: *start, Progress: progressPrinter("written"),
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		fmt.Printf("restored VM %q (%s) on %s\n", res.Name, res.ID, res.Host)
		for _, d := range res.Disks {
			fmt.Println("  disk", d)
		}
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		return nil
	}
	return fmt.Errorf("unknown hyperv command %q", args[0])
}
