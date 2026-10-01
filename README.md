# backupzit

Self-hosted backup for servers and workstations: file/folder and full image
backup with restore, managed from a central web console. Free and open source
(AGPLv3), with commercial support available.

Each organization runs its own backupzit server on its local network (no cloud
service, nothing exposed to the internet); agents on servers and workstations
connect to it over HTTPS.

> Status: **phase 2** — management console (agents, storage, jobs, schedules,
> history, restore) and agent packages (MSI, .deb, .rpm). File/folder backup only.

## Roadmap

1. **Core** — file/folder backup and restore, SFTP target, deduplication, integrity check ✅
2. **Management console** — agent registration, jobs, schedules, history, restore, agent downloads; Windows MSI and Linux packages ✅ (first version)
3. **Windows image backup** — VSS snapshots ✅, whole-disk / partition images ✅, file-level restore from images ✅
4. **Bare-metal restore** — image restore to an empty disk ✅ (verified booting); boot media, dissimilar hardware
5. **More targets** — S3 ✅, SMB ✅; retention policies ✅; email notifications ✅
6. **Security** — encryption, immutable repositories (S3 Object Lock, hardened Linux repository)
7. **Hypervisor (agentless) backup** — Proxmox, VMware
8. **Linux image backup**; more users, roles, optional LDAP login

Supported agent platforms (target): Windows 7, 10, 11, Windows Server 2008 R2+;
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

SFTP authentication: `--sftp-password` (or `BACKUPZIT_SFTP_PASSWORD`) and/or
`--sftp-key`. The server host key must be pinned with `--sftp-hostkey SHA256:...`;
on first connect without it, the agent prints the fingerprint the server presented.

## Repository format (v1)

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
```

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).
