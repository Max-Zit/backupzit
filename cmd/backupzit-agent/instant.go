package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/max-zit/backupzit/internal/agent"
	"github.com/max-zit/backupzit/internal/api"
	"github.com/max-zit/backupzit/internal/imaging"
	"github.com/max-zit/backupzit/internal/nbd"
	"github.com/max-zit/backupzit/internal/pve"
)

// cmdNBDServe serves the disks of a VM in a backup read-only over NBD on a
// unix socket (instant recovery; started as a systemd unit).
func cmdNBDServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("nbd-serve", flag.ExitOnError)
	rf := addRepoFlags(fs)
	snapshot := fs.String("snapshot", "latest", "backup")
	vmid := fs.Int("vmid", 0, "VM in the backup")
	socket := fs.String("socket", "", "unix socket to listen on")
	fs.Parse(args)
	if *vmid == 0 || *socket == "" {
		return errors.New("usage: backupzit-agent nbd-serve --repo R --snapshot ID --vmid N --socket PATH")
	}
	r, err := rf.open(ctx)
	if err != nil {
		return err
	}
	defer r.Close()
	sn, err := r.LoadSnapshot(ctx, *snapshot)
	if err != nil {
		return err
	}
	var exports []nbd.Export
	for _, g := range sn.Guests {
		if g.VMID != *vmid {
			continue
		}
		for _, d := range g.Disks {
			if d.Image < 0 || d.Image >= len(sn.Images) || len(sn.Images[d.Image].Partitions) != 1 {
				return fmt.Errorf("disk %s has no image in the backup", d.Key)
			}
			p := sn.Images[d.Image].Partitions[0]
			pr, err := imaging.NewPartitionReader(ctx, r, &p)
			if err != nil {
				return err
			}
			exports = append(exports, nbd.Export{Name: d.Key, Size: pr.Size(), Data: pr})
		}
	}
	if len(exports) == 0 {
		return fmt.Errorf("VM %d is not in backup %s", *vmid, sn.ID.Short())
	}
	os.Remove(*socket)
	l, err := net.Listen("unix", *socket)
	if err != nil {
		return err
	}
	os.Chmod(*socket, 0o600)
	srv := nbd.NewServer(exports...)
	srv.Log = func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM)
	defer stop()
	go func() { <-ctx.Done(); l.Close() }()
	fmt.Printf("serving %d disks of VM %d from backup %s on %s\n", len(exports), *vmid, sn.ID.Short(), *socket)
	return srv.Serve(l)
}

// cmdPVEInstant handles pve instant|instant-finish|instant-discard.
func cmdPVEInstant(ctx context.Context, sub string, args []string) error {
	switch sub {
	case "instant":
		fs := flag.NewFlagSet("pve instant", flag.ExitOnError)
		rf := addRepoFlags(fs)
		vmid := fs.Int("vmid", 0, "guest in the backup")
		newVMID := fs.Int("new-vmid", -1, "ID of the instant VM (-1: next free)")
		name := fs.String("name", "", "name of the VM")
		start := fs.Bool("start", true, "start the VM")
		fs.Parse(args)
		if fs.NArg() != 1 || *vmid == 0 {
			return errors.New("usage: backupzit-agent pve instant --repo R --vmid N [--new-vmid N] [--name X] SNAPSHOT|latest")
		}
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
		r.Close()
		if err != nil {
			return err
		}
		rs := api.Repository{URL: rf.location, Password: rf.password, SFTPPassword: rf.opts.SFTPPassword, SFTPHostKey: rf.opts.SFTPHostKey,
			S3AccessKey: rf.opts.S3AccessKey, S3SecretKey: rf.opts.S3SecretKey, S3Region: rf.opts.S3Region,
			SMBPassword: rf.opts.SMBPassword, SMBDomain: rf.opts.SMBDomain, HardenedKey: rf.opts.HardenedKey,
			HardenedFingerprint: rf.opts.HardenedFingerprint, AzureKey: rf.opts.AzureKey, AzureSAS: rf.opts.AzureSAS}
		if rf.opts.SFTPKeyFile != "" {
			b, err := os.ReadFile(rf.opts.SFTPKeyFile)
			if err != nil {
				return err
			}
			rs.SFTPKey = string(b)
		}
		res, err := pve.InstantStart(ctx, sn, pve.InstantOptions{VMID: *vmid, NewVMID: *newVMID, Name: *name, Start: *start,
			ServeCommand: func(ctx context.Context, target int, socket string) error {
				return agent.StartInstantServer(ctx, rs, sn.ID.String(), *vmid, target, socket)
			},
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		fmt.Printf("VM %d (%s) runs from backup %s on node %s", res.VMID, res.Name, sn.ID.Short(), res.Node)
		if res.Started {
			fmt.Print(", started")
		}
		fmt.Println()
		for _, d := range res.Disks {
			fmt.Println("  disk", d)
		}
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		fmt.Printf("finish with: backupzit-agent pve instant-finish --vmid %d --storage <storage>\n", res.VMID)
		return nil
	case "instant-finish":
		fs := flag.NewFlagSet("pve instant-finish", flag.ExitOnError)
		vmid := fs.Int("vmid", 0, "instant VM")
		storage := fs.String("storage", "", "storage for the disks")
		fs.Parse(args)
		if *vmid == 0 || *storage == "" {
			return errors.New("usage: backupzit-agent pve instant-finish --vmid N --storage S")
		}
		notes, err := pve.InstantFinish(ctx, *vmid, *storage, func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) })
		for _, n := range notes {
			fmt.Println("  note:", n)
		}
		if err != nil {
			return err
		}
		fmt.Printf("VM %d now runs from %s; the backup is no longer used\n", *vmid, *storage)
		return nil
	case "instant-discard":
		fs := flag.NewFlagSet("pve instant-discard", flag.ExitOnError)
		vmid := fs.Int("vmid", 0, "instant VM")
		fs.Parse(args)
		if *vmid == 0 {
			return errors.New("usage: backupzit-agent pve instant-discard --vmid N")
		}
		if err := pve.InstantDiscard(ctx, *vmid); err != nil {
			return err
		}
		fmt.Printf("VM %d deleted\n", *vmid)
		return nil
	}
	return fmt.Errorf("unknown command %q", sub)
}

// cmdESXiNFSServe serves the instant VMs of VMware ESXi hosts over NFS
// (started as a systemd unit by instant recovery runs).
func cmdESXiNFSServe(ctx context.Context) error {
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()
	return agent.ServeESXiInstant(ctx, func(f string, a ...any) { fmt.Fprintf(os.Stderr, f+"\n", a...) })
}
