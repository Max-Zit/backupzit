# backupzit

Self-hosted backup for servers and workstations: file/folder and full image
backup with restore, managed from a central web console. Free and open source
(AGPLv3), with commercial support available.

> Status: **phase 1** — command line agent with file/folder backup and restore
> to a local or SFTP repository.

## Roadmap

1. **Core** — file/folder backup and restore, SFTP target, deduplication, integrity check ✅
2. **Management console** — tenants (MSP), agent registration, jobs, schedules, history, agent downloads; Windows MSI and Linux packages
3. **Windows image backup** — VSS snapshots, volume images, file-level restore from images
4. **Bare-metal restore** — boot media, restore to same/different hardware or a Proxmox VM
5. **More targets** — S3, SMB; retention policies; notifications
6. **Security** — encryption, immutable repositories (S3 Object Lock, hardened Linux repository)
7. **Hypervisor (agentless) backup** — Proxmox, VMware
8. **Linux image backup**, SAML / LDAPS / roles

Supported agent platforms (target): Windows 7, 10, 11, Windows Server 2008 R2+;
AlmaLinux, Rocky Linux, Ubuntu, Debian.

## Agent CLI (phase 1)

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
go test ./...                      # end-to-end backup/restore tests (local + in-memory SFTP)
go build -o bin/ ./cmd/backupzit-agent
```

## License

GNU Affero General Public License v3.0 — see [LICENSE](LICENSE).
