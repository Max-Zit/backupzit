package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/max-zit/backupzit/internal/vmware"
)

type vmwareFlags struct {
	host, user, thumbprint, sshKey *string
}

func addVMwareFlags(fs *flag.FlagSet) vmwareFlags {
	return vmwareFlags{
		host:       fs.String("host", "", "ESXi host name or address"),
		user:       fs.String("user", "root", "ESXi user (password in BACKUPZIT_VMWARE_PASSWORD)"),
		thumbprint: fs.String("thumbprint", "", "expected HTTPS certificate fingerprint (SHA256:…), shown by 'vmware list'"),
		sshKey:     fs.String("ssh-host-key", "", "expected SSH host key fingerprint (SHA256:…)"),
	}
}

func (f vmwareFlags) connect(ctx context.Context) (*vmware.Client, error) {
	if *f.host == "" {
		return nil, errors.New("--host is required")
	}
	pw := os.Getenv("BACKUPZIT_VMWARE_PASSWORD")
	if pw == "" {
		return nil, errors.New("set the ESXi password in the environment variable BACKUPZIT_VMWARE_PASSWORD")
	}
	return vmware.Connect(ctx, vmware.Conn{Host: *f.host, User: *f.user, Password: pw, Thumbprint: *f.thumbprint, SSHHostKey: *f.sshKey})
}

// cmdVMware handles: vmware list|backup|restore (any agent as proxy for an ESXi host).
func cmdVMware(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backupzit-agent vmware list|backup|restore --host ESXI [options]")
	}
	switch args[0] {
	case "list":
		fs := flag.NewFlagSet("vmware list", flag.ExitOnError)
		vf := addVMwareFlags(fs)
		fs.Parse(args[1:])
		c, err := vf.connect(ctx)
		if err != nil {
			return err
		}
		defer c.Logout()
		vms, err := c.VMs(ctx)
		if err != nil {
			return err
		}
		mode := "API (licensed)"
		if !c.APIWrites {
			mode = "free license: snapshots and restores over SSH"
		}
		sshErr := c.CheckSSH(ctx)
		fmt.Printf("%s  ESXi %s, %s\n", *vf.host, c.Version, mode)
		fmt.Printf("  certificate %s\n", c.Thumbprint)
		if sshErr != nil {
			fmt.Printf("  SSH: %v\n", sshErr)
		} else {
			fmt.Printf("  SSH host key %s\n", c.SSHHostKey)
		}
		fmt.Printf("%-11s %-24s %-10s %10s  %-4s %-5s  %s\n", "VMID", "NAME", "STATE", "DISKS", "CBT", "TOOLS", "UUID")
		for _, v := range vms {
			fmt.Printf("%-11d %-24s %-10s %10s  %-4v %-5v  %s\n", v.VMID, v.Name, strings.TrimPrefix(v.Power, "powered"), humanBytes(v.DiskSize()), v.CBT, v.Tools, v.UUID)
		}
		dss, err := c.Datastores(ctx)
		if err == nil {
			for _, d := range dss {
				fmt.Printf("datastore %s: %s free of %s\n", d.Name, humanBytes(d.Free), humanBytes(d.Capacity))
			}
		}
		return nil
	case "backup":
		fs := flag.NewFlagSet("vmware backup", flag.ExitOnError)
		vf := addVMwareFlags(fs)
		rf := addRepoFlags(fs)
		ids := fs.String("vmid", "", "comma-separated VMIDs from 'vmware list' (default: all VMs)")
		fs.Parse(args[1:])
		var sel []int
		if *ids != "" {
			s, err := parseInts(*ids)
			if err != nil {
				return err
			}
			sel = s
		}
		c, err := vf.connect(ctx)
		if err != nil {
			return err
		}
		defer c.Logout()
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
		sn, err := c.Backup(ctx, r, vmware.BackupOptions{VMIDs: sel, Version: version, Progress: progressPrinter("read"),
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
		fs := flag.NewFlagSet("vmware restore", flag.ExitOnError)
		vf := addVMwareFlags(fs)
		rf := addRepoFlags(fs)
		vmid := fs.Int("vmid", 0, "VM in the backup (VMID from 'vmware list' or 'pve show')")
		asNew := fs.Bool("new", false, "create a second VM next to the original (new name, UUID and MAC addresses)")
		name := fs.String("name", "", "name of the restored VM")
		ds := fs.String("datastore", "", "datastore for the restored VM (default: the original one)")
		overwrite := fs.Bool("overwrite", false, "replace the disks of the original VM if it still exists (old disks are kept renamed)")
		start := fs.Bool("start", false, "start the VM after the restore")
		fs.Parse(args[1:])
		if fs.NArg() != 1 || *vmid == 0 {
			return errors.New("usage: backupzit-agent vmware restore --host ESXI [options] --vmid N <snapshot-id|latest>")
		}
		c, err := vf.connect(ctx)
		if err != nil {
			return err
		}
		defer c.Logout()
		r, err := rf.open(ctx)
		if err != nil {
			return err
		}
		defer r.Close()
		sn, err := r.LoadSnapshot(ctx, fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := c.Restore(ctx, r, sn, vmware.RestoreOptions{VMID: *vmid, AsNew: *asNew, Name: *name, Datastore: *ds,
			Overwrite: *overwrite, Start: *start, Progress: progressPrinter("written"),
			Log: func(msg string, kv ...any) { fmt.Println(append([]any{" ", msg}, kv...)...) }})
		if err != nil {
			return err
		}
		fmt.Printf("restored VM %q (ID %s) %s\n", res.Name, res.MoID, res.VMX)
		for _, d := range res.Disks {
			fmt.Println("  disk", d)
		}
		for _, n := range res.Notes {
			fmt.Println("  note:", n)
		}
		return nil
	}
	return fmt.Errorf("unknown vmware command %q", args[0])
}
