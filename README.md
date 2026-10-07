# BackupZit

**Self-hosted backup for servers, workstations, virtual machines, databases and Microsoft 365, managed from one web console.**

BackupZit backs up Windows and Linux machines (files or whole disks), Proxmox VE, Hyper-V and VMware ESXi virtual machines, SQL Server, PostgreSQL and MySQL/MariaDB databases, NAS shares and Microsoft 365. It stores the backups deduplicated, compressed and encrypted on storage you choose, and restores anything from a single file to a whole machine on new hardware.

- **Your own server, your own network.** Every organization runs its own console. There is no cloud service and nothing has to be exposed to the internet.
- **Free and open source** under the AGPLv3. [MaxZit](https://github.com/Max-Zit) offers commercial support.
- The console is available in 14 languages: English, German, French, Spanish, Italian, Portuguese, Dutch, Polish, Czech, Serbian, Turkish, Russian, Chinese and Japanese.

**Download:** [latest release](https://github.com/Max-Zit/backupzit/releases/latest) · **Install:** [quick start](#installation) · **Guide:** built into the console under *Documentation*

---

## Contents

- [What it can back up](#what-it-can-back-up)
- [How it works](#how-it-works)
- [Installation](#installation)
- [First steps](#first-steps)
- [Installing agents](#installing-agents)
- [Updating](#updating)
- [Supported systems](#supported-systems)
- [Agent CLI (standalone)](#agent-cli-standalone)
- [Repository format](#repository-format-v1)
- [Development](#development) · [License](#license)

## What it can back up

| What | How | Restore |
|---|---|---|
| **Files and folders** (Windows, Linux) | incremental, from a VSS snapshot on Windows | single files from the browser (download), into a folder or to the original location |
| **Whole disks** (Windows) | disk images of one, several or all disks, changed blocks only | bare metal from the recovery ISO, also onto different hardware; single files from the image |
| **Linux systems** | all file systems and the disk layout | bare metal from the Linux recovery ISO, or as a new Proxmox VM (P2V) |
| **Proxmox VE** VMs and containers | agentless, changed blocks only | new VM or in place, **instant recovery** (the VM runs from the backup in seconds), replication, automatic boot tests |
| **VMware ESXi** VMs | agentless through a proxy agent, changed block tracking | new VM or in place, instant recovery over NFS |
| **Hyper-V** VMs | agentless, resilient change tracking | new VM on the same or another Hyper-V host |
| **Databases** | SQL Server, PostgreSQL, MySQL/MariaDB with native tools; log backups | a single database to any point in time, under a new name or in place |
| **NAS shares** | SMB or NFS share read by any agent, nothing installed on the NAS | into a folder or back onto the share |
| **Microsoft 365** | Exchange Online mail (.eml), calendars (.ics), contacts (.vcf), OneDrive and SharePoint through Microsoft Graph, read-only permissions | browse and download, or restore into a folder |

**Storage:** SFTP, S3-compatible storage (AWS, Wasabi, Backblaze B2, MinIO, Google Cloud Storage), SMB shares, Azure Blob Storage, rotating USB disks, local disks, and the **BackupZit hardened repository** (a write-once Linux server). Backup copy jobs keep a second copy elsewhere (3-2-1).

**Protection against ransomware and mistakes:**
- AES-256 encryption with a recovery key.
- Immutable backups: S3 Object Lock or the hardened repository.
- Detection of backups that look like ransomware or mass deletion; retention pauses automatically.
- Automatic restore tests.
- Optional four-eyes approval for destructive changes.
- Tamper-evident audit log.

**Management:**
- Schedules and retention per job.
- Email, Slack, Teams and webhook notifications.
- Reports (CSV, emailed on a schedule) and a calendar of runs.
- Roles (Administrator, Backup operator, Restore operator, Viewer).
- LDAP / Active Directory and single sign-on (SAML, OpenID Connect).
- Two-factor sign-in and a REST API.
- Signed self-updates of the console and of the agents.
- A backup of the console itself.

## How it works

```
                    ┌──────────────────────────────┐
   your browser ───►│  BackupZit console           │  web console, jobs, schedules,
   (HTTPS 8443)     │  (Linux VM + PostgreSQL)     │  users, history, updates
                    └──────────────▲───────────────┘
                                   │ agents ask for work (HTTPS 8443, outbound only)
        ┌──────────────────────────┼──────────────────────────┐
   ┌────┴─────┐              ┌─────┴─────┐              ┌──────┴──────┐
   │ Windows  │              │  Linux    │              │ Proxmox /   │   ESXi hosts through
   │ agent    │              │  agent    │              │ Hyper-V     │   a proxy agent,
   └────┬─────┘              └─────┬─────┘              └──────┬──────┘   M365 through any agent
        └──────────────────────────┼──────────────────────────┘
                                   ▼  agents write directly to the storage
          SFTP · S3 · SMB · Azure · USB · hardened repository
```

- **The console** is a Linux server, usually a small VM (2 vCPU, 2–4 GB RAM, 20 GB disk). It does not store backup data.
- **Agents** run as a service on every protected machine. They connect to the console (no open ports on the clients), get their jobs, and write backups **directly to the storage**.
- Agents trust the console by its certificate fingerprint (pinned when they enroll), so no public certificate authority is needed.

## Installation

Choose one of four ways. The first one is the fastest.

| Way | Best for | Needs |
|---|---|---|
| **1. Ready-made VM image** (OVA, qcow2, VHDX) | import, start, sign in, in a few minutes | VMware, Proxmox/KVM or Hyper-V; no internet access |
| **2. Appliance ISO** | a new VM or a physical server, unattended | internet access during installation |
| **3. Install script** | an existing Debian, Ubuntu, AlmaLinux or Rocky Linux server | root and internet access |
| **4. Packages by hand** | your own PostgreSQL, configuration management, offline servers | the server package from the release |

**Requirements:**
- A VM or server with 2 vCPU, 2–4 GB RAM and a 16–20 GB disk.
- Agents must reach the console on **TCP 8443**, and the storage directly.

### Downloads

These links always point to the newest release:

| File | What |
|---|---|
| [backupzit-appliance.ova](https://github.com/Max-Zit/backupzit/releases/latest/download/backupzit-appliance.ova) | VM image for VMware ESXi, Workstation, VirtualBox |
| [backupzit-appliance.qcow2](https://github.com/Max-Zit/backupzit/releases/latest/download/backupzit-appliance.qcow2) | VM image for Proxmox VE and KVM |
| [backupzit-appliance-vhdx.zip](https://github.com/Max-Zit/backupzit/releases/latest/download/backupzit-appliance-vhdx.zip) | VM image for Hyper-V (generation 1) |
| [backupzit-appliance.iso](https://github.com/Max-Zit/backupzit/releases/latest/download/backupzit-appliance.iso) | appliance installer ISO |
| [install.sh](https://github.com/Max-Zit/backupzit/releases/latest/download/install.sh) | install script for an existing Linux server |
| [SHA256SUMS](https://github.com/Max-Zit/backupzit/releases/latest/download/SHA256SUMS) | checksums of the images and the ISO |

The agent installers (MSI, deb, rpm), the server packages and the Linux recovery ISO are listed on the [release page](https://github.com/Max-Zit/backupzit/releases/latest).

### 1. Ready-made VM image

1. Import the image:
   - **VMware ESXi:** in the Host Client choose *Create / Register VM → Deploy a virtual machine from an OVF or OVA file* and select `backupzit-appliance.ova`. On vSphere use *Deploy OVF Template*.
   - **Proxmox VE:** create a VM without a disk (2 vCPU, 2–4 GB RAM, VirtIO network). Copy `backupzit-appliance.qcow2` to the node, then run:
     ```
     qm importdisk <vmid> backupzit-appliance.qcow2 local-lvm
     ```
     Attach the imported disk as SCSI, make it the boot disk and enable the QEMU guest agent.
   - **Hyper-V:** unzip `backupzit-appliance-vhdx.zip`. Create a *generation 1* VM with 2–4 GB RAM and choose the VHDX as its disk.
2. Start the VM. Its screen shows **the address of the web console and the initial password** of the user `admin`.
3. Open `https://<address>:8443` in a browser. The browser warns once about the self-signed certificate. Sign in as `admin` and change the password.

The appliance gets its address by DHCP. On the VM's own screen, sign in as `bzadmin` (initial password `backupzit`, which must be changed). A setup menu opens, where you can set:
- a static address,
- the server name,
- the time zone (e.g. `Europe/Belgrade`; the appliance starts in UTC),
- SSH on or off.

### 2. Appliance ISO

1. Create a VM or use a server with 2 vCPU, 2–4 GB RAM and a 16 GB or larger disk.
2. Boot it from `backupzit-appliance.iso`. After 5 seconds the installation starts by itself:
   - it **erases the first disk**;
   - it installs Debian, PostgreSQL and the console;
   - it takes about 10–20 minutes and needs internet access.
3. After the restart, the screen shows the console's address and the initial `admin` password, as with the VM image.

### 3. Install script (existing Linux server)

On Debian 12+, Ubuntu 22.04+, AlmaLinux or Rocky Linux 9+ (x86-64), as root:

```
curl -fsSL https://github.com/Max-Zit/backupzit/releases/latest/download/install.sh | sudo sh
```

The script:
- installs PostgreSQL;
- downloads the newest release and verifies its signature and checksum;
- installs the console and creates the database with a random password;
- turns on automatic security updates;
- opens port 8443 in the firewall;
- prints the console's address and the initial `admin` password.

Options go after `sh -s --`:

| Option | Effect |
|---|---|
| `--version 0.35.2` | install this release instead of the newest |
| `--db postgres://user:pw@host:5432/db` | use an existing PostgreSQL database |
| `--hosts backup.example.local,10.0.0.10` | names and addresses for the console's certificate |
| `--no-auto-updates` | leave automatic security updates as they are |

Running the script again updates BackupZit and keeps the configuration and data. To read the script before running it, use `curl -fsSLO …/install.sh; less install.sh; sudo sh install.sh`.

### 4. Packages by hand

**Debian / Ubuntu**

```
apt install postgresql
apt install ./backupzit-server_*.deb
```

**AlmaLinux / Rocky Linux**

```
dnf install postgresql-server && postgresql-setup --initdb && systemctl enable --now postgresql
dnf install ./backupzit-server-*.rpm
```

On AlmaLinux and Rocky Linux, PostgreSQL must accept password sign-in:
1. In `/var/lib/pgsql/data/pg_hba.conf`, change `ident` to `scram-sha-256` on the `127.0.0.1` and `::1` lines.
2. Restart PostgreSQL.

**Create the database and start the console:**

```
sudo -u postgres psql -c "SET password_encryption = 'scram-sha-256'; CREATE USER backupzit WITH PASSWORD 'choose-a-strong-password'"
sudo -u postgres psql -c "CREATE DATABASE backupzit OWNER backupzit"
```

Write the settings into `/etc/backupzit/server.env`:

```
BACKUPZIT_DB=postgres://backupzit:choose-a-strong-password@localhost:5432/backupzit?sslmode=disable
BACKUPZIT_PUBLIC_URL=https://backup.example.local:8443
BACKUPZIT_TLS_HOSTS=backup.example.local,10.0.0.10
```

Then start the console:

```
systemctl start backupzit-server
```

The initial `admin` password is in `/var/lib/backupzit/initial-admin-password.txt`. To reset a forgotten password, run on the server:

```
set -a; . /etc/backupzit/server.env; set +a
backupzit-server --set-admin-password 'NEW-PASSWORD'
```

**Just trying it out?** This runs a private PostgreSQL in the data folder and serves the console over plain HTTP on localhost (Windows or Linux, no installation):

```
backupzit-server --dev-embedded-db --data-dir ./bzdata --dev-http 127.0.0.1:8080
```

## First steps

1. **Sign in** at `https://<console>:8443` as `admin` and change the password (*My account*).
2. **Add storage:** open *Storage → Add storage target* and choose, for example, an SFTP server, S3 bucket, SMB share or hardened repository. The console tests the connection when you save. Write down the **recovery key** it shows: with it, backups can be restored even without the console.
3. **Enroll agents:** on *Agents* click *Create enrollment token* and install the agent on each machine with the code (see [Installing agents](#installing-agents)). Machines appear within a minute.
4. **Create a backup job** under *Backup jobs*: choose the agent, what to back up, the storage, a schedule (e.g. daily at 22:00) and the retention (e.g. 14 daily, 8 weekly, 12 monthly).
5. Optionally:
   - email notifications under *Settings → Email*;
   - a trusted HTTPS certificate under *Settings → Certificate* (upload or Let's Encrypt);
   - the time zone under *Settings → Network*.

The complete guide (all job kinds, restores, bare-metal recovery, security, troubleshooting) is built into the console under **Documentation**.

## Installing agents

On the console's *Agents* page, *Create enrollment token* shows an **enrollment code** (`BZ1-…`). One code is valid for 7 days and works for any number of machines.

**Windows** (10, 11, Server 2016 and newer; Windows 7 / 2008 R2 / 2012 R2 use the `-legacy.msi`):
- Run the MSI and paste the code when the installer asks for it.
- Silent install (GPO, Intune, RMM):

```
msiexec /i backupzit-agent-<version>-x64.msi /qn ENROLLCODE="BZ1-..."
```

**Linux** (Debian, Ubuntu, AlmaLinux, Rocky Linux, Proxmox VE nodes):

```
apt install ./backupzit-agent_*.deb        # or: dnf install ./backupzit-agent-*.rpm
backupzit-agent enroll --code "BZ1-..."
systemctl enable --now backupzit-agent
```

**From the console:** under *Agents → Install on a machine from here*, the console installs the agent itself, over SSH (Linux) or WinRM (Windows).

**VMware ESXi:** nothing is installed on the host. Add it under *Agents → VMware ESXi hosts* and choose an agent as its proxy.

## Updating

**Console.** It checks the official releases every day; *Settings → Updates → Install* updates it:
- every release is signed, and the console verifies the signature;
- the database is backed up first;
- if the new version does not start, the previous version and database are restored automatically.

Without internet access, upload the release files on the same page. Updating with the package manager or by running the install script again also works.

**Agents** are updated by the console, automatically after a console update or with *Update all agents*. A running backup is never interrupted.

**Operating system** updates of the appliance are shown and installed under *Settings → Updates* as well.

## Supported systems

| Component | Systems |
|---|---|
| Console | Debian 12+, Ubuntu 22.04+, AlmaLinux / Rocky Linux 9+ (x86-64), PostgreSQL 13+; or the ready-made appliance |
| Windows agent | Windows 10, 11, Server 2016 and newer (MSI); Windows 7 SP1, Server 2008 R2 SP1, 2012 R2 (legacy MSI) |
| Linux agent | Debian, Ubuntu, AlmaLinux, Rocky Linux (64-bit), Proxmox VE 7 and 8 |
| Hypervisors | Proxmox VE 7/8, Hyper-V (Windows Server 2016+, Windows 10/11), VMware ESXi 6.7, 7, 8 (also with the free license) |
| Microsoft 365 | any tenant with an app registration (application permissions, read-only) |

**Planned:** vCenter, instant recovery for Hyper-V, restore directly into Microsoft 365 mailboxes, Teams chats.

## Agent CLI (standalone)

The agent also works without a console, from the command line:

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
    backupzit-agent restore   --repo s3://... --as-of 2026-10-01T14:30 --target D:\Restore latest

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
