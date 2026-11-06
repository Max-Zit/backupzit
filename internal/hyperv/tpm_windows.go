package hyperv

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/max-zit/backupzit/internal/repo"
)

// maxGuestState bounds the guest state file read into memory (Hyper-V
// creates them with 4 MiB).
const maxGuestState = 256 << 20

// saveTPM stores the vTPM of a Generation 2 VM: its guest state file (VMGS,
// which holds the vTPM and the UEFI variables) as a blob, and its key
// protector, in vm.GuestState and vm.KeyProtector.
func saveTPM(ctx context.Context, r *repo.Repository, vm *vmInfo) error {
	out, err := ps(ctx, `$vm = Get-VM -Id `+psq(vm.ID)+`; [pscustomobject]@{ kp = [Convert]::ToBase64String((Get-VMKeyProtector -VM $vm));
 gs = "$(@(Get-ChildItem (Join-Path $vm.ConfigurationLocation 'Virtual Machines') -Filter ($vm.Id.Guid + '.vmgs'))[0].FullName)" } | ConvertTo-Json -Compress`)
	if err != nil {
		return err
	}
	var info struct{ KP, GS string }
	if err := json.Unmarshal(out, &info); err != nil {
		return err
	}
	if info.KP == "" || info.GS == "" {
		return errors.New("no key protector or guest state file")
	}
	f, err := os.Open(info.GS) // shared with the running VM
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxGuestState+1))
	if err != nil {
		return err
	}
	if len(data) > maxGuestState {
		return fmt.Errorf("guest state file %s is larger than %d MiB", info.GS, maxGuestState>>20)
	}
	id, _, err := r.SaveBlob(ctx, repo.DataBlob, data)
	if err != nil {
		return err
	}
	vm.GuestState, vm.KeyProtector = id.String(), info.KP
	return nil
}

// tpmRestoreScript returns the PowerShell that gives the new VM $vm its
// vTPM: the one of the backup when this host has the guardian of the
// original host (it prints TPM:kept), otherwise a new, empty one (TPM:new:
// and the reason). setup goes where the VM is configured, finish after the
// last Set-VMFirmware; cleanup removes the temporary files.
func tpmRestoreScript(ctx context.Context, r *repo.Repository, cfg vmInfo, dir string) (setup, finish string, cleanup func(), err error) {
	newTPM := `Set-VMKeyProtector -VM $vm -NewLocalKeyProtector; Enable-VMTPM -VM $vm; `
	cleanup = func() {}
	if cfg.GuestState == "" || cfg.KeyProtector == "" {
		return newTPM + `'TPM:new:the backup has no vTPM state (made before BackupZit 0.48)'; `, "", cleanup, nil
	}
	id, err := repo.ParseID(cfg.GuestState)
	if err != nil {
		return "", "", cleanup, err
	}
	gs, err := r.LoadBlob(ctx, repo.DataBlob, id)
	if err != nil {
		return "", "", cleanup, fmt.Errorf("read the vTPM state: %w", err)
	}
	kp, err := base64.StdEncoding.DecodeString(cfg.KeyProtector)
	if err != nil {
		return "", "", cleanup, err
	}
	gsFile := filepath.Join(dir, ".backupzit-restore.vmgs")
	kpFile := filepath.Join(dir, ".backupzit-restore.kp")
	cleanup = func() { _ = os.Remove(gsFile); _ = os.Remove(kpFile) }
	if err := os.WriteFile(gsFile, gs, 0o600); err != nil {
		return "", "", cleanup, err
	}
	if err := os.WriteFile(kpFile, kp, 0o600); err != nil {
		return "", "", cleanup, err
	}
	first := `("$($_.Exception.Message)" -split "` + "`" + `n")[0]`
	setup = fmt.Sprintf(`$bzTPM = ''; try { Set-VMKeyProtector -VM $vm -KeyProtector ([IO.File]::ReadAllBytes(%s)) -ErrorAction Stop; Enable-VMTPM -VM $vm -ErrorAction Stop; $bzTPM = 'kept' }
 catch { 'TPM:new:' + %s; %s}; `, psq(kpFile), first, newTPM)
	// The guest state file also holds the UEFI settings: it replaces the new
	// VM's file after every Set-VMFirmware.
	finish = fmt.Sprintf(`if ($bzTPM -eq 'kept') { try { Copy-Item %s @(Get-ChildItem (Join-Path $vm.ConfigurationLocation 'Virtual Machines') -Filter ($vm.Id.Guid + '.vmgs'))[0].FullName -Force -ErrorAction Stop; 'TPM:kept' }
 catch { 'TPM:new:' + %s } }; `, psq(gsFile), first)
	return setup, finish, cleanup, nil
}
