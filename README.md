# BackupZit

Self-hosted backup for servers and workstations: file/folder and full image
backup with restore, managed from a central web console. Free and open source
(AGPLv3), with commercial support available.

Each organization runs its own BackupZit server on its local network (no cloud
service, nothing exposed to the internet); agents on servers and workstations
connect to it over HTTPS.

> Status: **phase 2** — management console (agents, storage, jobs, schedules,
> history, restore) and agent packages (MSI, .deb, .rpm). File/folder backup only.

## Roadmap

1. **Core** — file/folder backup and restore, SFTP target, deduplication, integrity check ✅
2. **Management console** — agent registration, jobs, schedules, history, restore wizard, agent downloads and self-update; Windows MSI and Linux packages ✅
3. **Windows image backup** — VSS snapshots ✅, whole-disk / partition images ✅, file-level restore from images ✅
4. **Bare-metal restore** — image restore to an empty disk ✅, Windows (WinPE) and Linux recovery ISOs ✅, dissimilar hardware ✅
5. **More targets** — S3 ✅ (incl. Google Cloud Storage), SMB ✅, Azure Blob ✅, rotating USB disks ✅; retention policies ✅; storage usage charts and forecast ✅; email notifications ✅
6. **Security** — encryption ✅, immutable repositories (S3 Object Lock ✅, hardened Linux repository ✅), encrypted secrets in the console database ✅, two-factor sign-in ✅, brute-force protection ✅, trusted HTTPS certificates (upload / Let's Encrypt) ✅
7. **Hypervisor (agentless) backup** — Proxmox VE ✅, Hyper-V ✅ (incl. RCT), VMware ESXi ✅ (incl. changed block tracking); single files from VM disks ✅; instant VM recovery, boot tests and replication on Proxmox VE ✅
8. **Users** — local accounts, roles (Administrator, Backup operator, Restore operator, Viewer), LDAPS/Active Directory sign-in, four-eyes approval, audit log ✅; reports, calendar, in-console documentation ✅; console in English and Serbian ✅
9. **Linux system backup** — whole systems with disk layout, restore to bare metal or as a Proxmox VM (P2V) ✅; **NAS shares** (SMB, NFS) through an agent ✅; **databases** — Microsoft SQL Server, PostgreSQL and MySQL/MariaDB: native full and log backups, point-in-time restore of single databases ✅
10. **Appliance** — installer ISO and OVA/qcow2/VHDX images ✅, trusted HTTPS certificates ✅, signed console self-update with rollback ✅, OS updates from the console ✅, backup and restore of the console itself ✅
11. **Later** — vCenter, instant recovery for Hyper-V and VMware

Supported agent platforms: Windows 10, 11, Server 2016+ (MSI); Windows 7, Server 2008 R2/2012 R2
(legacy MSI built with the go-legacy-win7 toolchain: `build-msi.ps1 -Legacy`);
AlmaLinux, Rocky Linux, Ubuntu, Debian.

## Management server

```
backupzit-server --db postgres://user:pass@host/db [--listen :8443] [--data-dir DIR]
                 [--public-url https://backup.example.com:8443] [--tls-hosts names,ips]
```

- On first start it creates a self-signed TLS certificate in the data directory.
  Agents pin its SHA-256 fingerprint when enrolling, so no public CA is needed.
- User `admin` is created on first start; the password comes from
  `BACKUPZIT_ADMIN_PASSWORD` or is written to `<data-dir>/initial-admin-password.txt`.
  Reset it with `backupzit-server --db ... --set-admin-password NEW`.
- Agent installers placed in `<data-dir>/dist/` are offered for download in the console.
- For evaluation: `backupzit-server --dev-embedded-db --dev-http 127.0.0.1:8080` runs a
  private PostgreSQL in the data directory and serves the UI over plain HTTP on localhost.

The console includes a user guide under *Documentation*, reports (any period, CSV,
emailed and scheduled), a calendar of past and planned runs, and user management with
roles and optional LDAPS sign-in (Settings → LDAP; role from directory groups).

Agents poll the server over HTTPS (outbound only — no ports open on clients), receive
backup/restore runs, write directly to the storage target and report results. Each
agent gets its own repository: `<target>/<hostname>_<id>`.

## Agent installation

Windows (MSI, installs the `backupzit-agent` service running as LocalSystem):

```
msiexec /i backupzit-agent-<ver>-x64.msi /qn SERVER="https://backup:8443" TOKEN="..." FINGERPRINT="SHA256:..."
```

Linux (`.deb` for Ubuntu/Debian, `.rpm` for Alma/Rocky; systemd service):

```
backupzit-agent enroll --server https://backup:8443 --token ... --fingerprint SHA256:...
systemctl restart backupzit-agent
```

The console shows the exact commands with token and fingerprint filled in.

## Agent CLI (standalone)

```
backupzit-agent init      --repo <location>
backupzit-agent backup    --repo <location> [--exclude PATTERN] [--tag TAG] <path>...
backupzit-agent snapshots --repo <location>
backupzit-agent ls        --repo <location> <snapshot|latest>
backupzit-agent restore   --repo <location> (--target DIR | --original) [--include PATH] [--verify] <snapshot|latest>
backupzit-agent check     --repo <location> [--read-data]
```

Repository locations:

- `D:\backups\repo` or `/srv/backups/repo` — local directory
- `sftp://user@host[:port]/path` — SFTP (`/~/path` for a path relative to the home directory)
- `smb://[domain;]user@host/share[/path]` — SMB 2/3 share (Windows server, NAS, Samba); password via `BACKUPZIT_SMB_PASSWORD`
- `s3://endpoint[:port]/bucket[/prefix]` — S3 compatible storage (AWS, Wasabi, Backblaze B2, MinIO, …); add `?tls=false` for plain HTTP. Credentials via `BACKUPZIT_S3_ACCESS_KEY` / `BACKUPZIT_S3_SECRET_KEY`
- `usb://LABEL/path` — removable disk found by its volume label (wildcards: `usb://BZBACKUP*/BackupZit`)
- `azure://account/container[/path]` — Azure Blob Storage; key via `BACKUPZIT_AZURE_KEY` or SAS token via `BACKUPZIT_AZURE_SAS`; `?endpoint=URL` for Azure Stack/sovereign clouds/Azurite
- `hardened://host[:port]/path` — BackupZit hardened repository (port 8500 by default); access key via `BACKUPZIT_HARDENED_KEY`, certificate pin via `BACKUPZIT_HARDENED_FINGERPRINT`

SFTP authentication: `--sftp-password` (or `BACKUPZIT_SFTP_PASSWORD`) and/or
`--sftp-key`. The server host key must be pinned with `--sftp-hostkey SHA256:...`;
on first connect without it, the agent prints the fingerprint the server presented.

### Immutable backups (S3 Object Lock)

Create the bucket with Object Lock enabled (e.g. `mc mb --with-lock`, or the
"Object Lock" option on AWS / Wasabi / Backblaze B2) and enable *Immutable backups*
on the storage target (CLI: `--s3-lock-days N`). Every object except repository
lock files is written in **compliance mode** for N days (+1 day margin): nobody,
not even an attacker holding the storage keys or the bucket owner, can delete or
overwrite it before then. After each backup the agent extends the lock of older
data the new backup reuses (deduplication), so every restore point stays fully
protected for N days.

Deleting (retention, prune, an attacker) only adds delete markers. Add a lifecycle
rule that expires noncurrent versions (e.g. `mc ilm rule add --noncurrent-expire-days 1
--expire-delete-marker`) so space is freed once locks end.

If backups were deleted or overwritten, read the repository as it was before: (also works for the hardened repository)

    backupzit-agent snapshots --repo s3://... --as-of 2026-10-01T14:30
    backupzit-agent restore   --repo s3://... --as-of 2026-10-01T14:30 --target D:Restore latest

The point-in-time view is read-only and uses object versions, so nothing on the
storage changes.

### Hardened repository (backupzit-repo)

An immutable, write-once backup server for a Linux machine (Veeam-style
"hardened repository"), installed from the `backupzit-repo` deb/rpm package:

    apt install ./backupzit-repo_*.deb          # or dnf install ./backupzit-repo-*.rpm
    vi /etc/backupzit-repo/repo.env             # immutability period, storage directory
    systemctl start backupzit-repo
    backupzit-repo add-key office-console       # access key (shown once)
    backupzit-repo fingerprint                  # certificate fingerprint

Add a *Hardened repository* storage target in the console with the server
address, access key and fingerprint.

- Files are **write-once**: existing files are never overwritten.
- Every file gets the filesystem immutable attribute (`chattr +i`) for the
  period configured **on the repository server** (+1 day). Not even root can
  delete or change it without removing the attribute first. The service runs as
  an unprivileged user whose only capability is `CAP_LINUX_IMMUTABLE`.
- Deletes from clients (retention, prune — or an attacker with the access key)
  only **hide** files until their period ends; then they are removed.
  `backupzit-repo status` shows hidden data, `backupzit-repo undelete --since TIME`
  brings it back, and `--as-of TIME` on the agent CLI reads the state before.
- After setup, disable SSH and other remote access to the server and protect its
  console (iDRAC/iLO/hypervisor) — the immutability is only as strong as the
  root account of that machine.

## Repository format (v1)

Encrypted repositories (default for new storage targets in the console) seal every
blob, pack header, index and snapshot with AES-256-GCM under a random master key; the
master key and chunker polynomial are stored in `keys/<id>`, sealed with a key derived
from the recovery key via Argon2id. The plaintext `config` only states the scheme.
With the recovery key (console: Storage → Recovery key) backups can be restored with
the CLI alone: `BACKUPZIT_PASSWORD=<key> backupzit-agent restore --repo ...`.

```
config                 repository id, format version, chunker polynomial
data/<xx>/<pack-id>    pack files holding blobs
index/<index-id>       blob -> pack/offset map (accelerator; rebuildable)
snapshots/<id>         one JSON document per backup run
```

- Files are split with content-defined chunking (Rabin, 512 KiB – 8 MiB,
  ~1 MiB average). A chunk is identified by the SHA-256 of its content, so
  identical data is stored once across all files, snapshots and machines that
  share the repository.
- Directories are stored as JSON *tree* blobs (names, types, sizes, mtimes,
  Windows attributes, chunk lists).
- Blobs are zstd-compressed when that makes them smaller and grouped into
  ~16 MiB pack files. Each pack ends with a header listing its blobs, so the
  index can always be rebuilt from the packs themselves.
- Every object is named by its SHA-256; every read is verified against it.
- Writes are atomic (temp file + rename). Order: packs → index → snapshot, so
  an interrupted backup never produces a snapshot that references missing data.
- Incremental backups skip files whose size and mtime are unchanged since the
  parent snapshot; changed files are re-read but only new chunks are uploaded.
  Every snapshot is a full, independent restore point.

## Development

```
go test ./...                      # end-to-end tests (local + in-memory SFTP, server + embedded PostgreSQL)
go build -o bin/ ./cmd/...                     # agent + server
.\packaging\windows\build-msi.ps1 -Version X.Y.Z   # Windows MSI (WiX 5)
sh packaging/linux/build-packages.sh X.Y.Z       # .deb/.rpm (nfpm)
sh packaging/appliance/build-iso.sh dist/backupzit-server_X.Y.Z_amd64.deb   # appliance ISO (on Debian: xorriso, isolinux)
sh packaging/appliance/build-images.sh dist/backupzit-server_X.Y.Z_amd64.deb   # OVA, qcow2, VHDX (on a Proxmox VE host)
sh packaging/publish-release.sh X.Y.Z /path/to/release.key notes.txt   # sign and publish as GitHub release (consoles update from it)
go run ./tools/release sign -key release.key -version X.Y.Z -dir <release folder>   # signed update manifest
```

Releases for the console's self-update are signed with the Ed25519 release key; its public
key is built into `internal/update` (`TrustedKeys`). Keep `release.key` offline and backed up:
without it no update can be published to existing consoles.

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE). BackupZit is free to install and use; MaxZit offers commercial support.

Third-party components keep their own licenses (MIT, BSD, Apache 2.0, ISC, MPL 2.0, zlib, public domain — all compatible with the AGPL). Their texts are in [THIRD_PARTY_LICENSES.txt](THIRD_PARTY_LICENSES.txt), which every package installs; regenerate it with `go run ./tools/licenses > THIRD_PARTY_LICENSES.txt` (it fails on a license that is not known to be compatible). `internal/vss/vss_windows.go` is adapted from restic under the BSD 2-Clause License; its header keeps the original copyright.
