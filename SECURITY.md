# Security policy

## Supported versions

Security fixes are released for the newest BackupZit version. Consoles check for updates and install signed releases from *Settings → Updates*; agents are updated from the *Agents* page.

## Reporting a vulnerability

Please report security problems privately, not in a public issue:

1. Open the [Security tab](https://github.com/Max-Zit/backupzit/security) of this repository.
2. Click **Report a vulnerability** and describe the problem, the affected version and how to reproduce it.

We confirm reports within a few working days, fix confirmed problems in a new release and credit the reporter in the release notes unless they prefer otherwise.

## What to know when you set it up

- **The console controls the agents.** Whoever controls the console — an administrator account or the console server itself — can run backups, restores that overwrite files and the commands of jobs as root or SYSTEM on every agent. Protect it like other systems with that reach: two-factor sign-in, four-eyes approval, web access only from the administrators' network, and the console server patched and not shared with other services.
- **Machines on one storage target share it.** Agents that back up to the same target use its credentials and its recovery key, so root or SYSTEM on one of them can read the other machines' backups there and, without immutable storage, delete them. Use immutable storage (the hardened repository or S3 Object Lock), so a compromised machine cannot delete backups, and separate targets with their own account and key for machines that must not trust each other.
- **A suspicious backup pauses more than retention.** When a backup looks like ransomware or mass deletion, the job removes no older backups, copy jobs after each backup and replication do not start, and copy jobs remove no older copies until someone has checked the machine.

## How releases are protected

- Every release is signed with the BackupZit release key; consoles verify the signature and checksums before installing an update. Agents download updates only from their console over the pinned connection. Since 0.56 they install an update only if the installer is in the list of agent installers signed with the release key (checked with the key built into the agent) and is newer than the agent; a changed installer on the console is refused. Older agents check only the SHA-256 checksum the console gives.
- Release packages are built from tagged commits of this repository; the appliance and recovery images carry a written source offer (`SOURCE-CODE.txt`).

## How BackupZit is tested

**On every change (GitHub Actions, results in the [Security tab](https://github.com/Max-Zit/backupzit/security)):**

| Check | What it finds |
|---|---|
| [CI](https://github.com/Max-Zit/backupzit/actions/workflows/ci.yml): `go vet` and the full test suite on Linux, build for Windows | regressions; the tests run the console against an embedded PostgreSQL and in-process SFTP, S3 and vSphere simulators |
| The whole test suite again with the Go **race detector** | data races between backups, schedules and web requests |
| **Fuzzing** of every parser that reads untrusted input: repository pack headers, index files and compressed blobs, VMX files and datastore paths, VMFS/LVM metadata, update manifests and signatures, enrollment codes, snapshot paths, network files, hardened-repository object names and access-key files, schedules, redirect targets, SSO state, MFA tokens, SAML metadata and responses, and uploaded certificates | crashes, memory exhaustion and checks that can be bypassed |
| [gosec](https://github.com/securego/gosec) for Linux and Windows | insecure Go patterns: command execution, file permissions, weak cryptography; every accepted finding carries a written reason in the code |
| [staticcheck](https://staticcheck.dev/) for Linux and Windows | bugs and dead code |
| [govulncheck](https://go.dev/doc/security/vuln/) | known vulnerabilities in Go and in the dependencies the code actually calls |
| [CodeQL](https://codeql.github.com/) (Go, JavaScript, workflows) | injection, path traversal, unsafe redirects and other data-flow bugs |
| [Dependabot](https://docs.github.com/code-security/dependabot) | vulnerable or outdated Go modules and actions |
| Secret scanning with push protection | credentials committed by mistake |
| [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/Max-Zit/backupzit) | supply-chain practices of the repository |

The release branch accepts only commits that pass the build, tests, race detector, fuzzing, gosec, staticcheck and govulncheck.

**Before every release (in a test lab, on the exact release candidate):**

| Check | What it finds |
|---|---|
| [Semgrep](https://semgrep.dev/) (Go, JavaScript, secrets, OWASP Top 10 rules) | injection, XSS, insecure cookies, open redirects |
| [Trivy](https://trivy.dev/) | vulnerable dependencies, secrets and misconfigurations |
| [Gitleaks](https://gitleaks.io/) | secrets in the whole git history |
| [OWASP ZAP](https://www.zaproxy.org/) full scan, signed in as an administrator | XSS, SQL injection, CSRF, path traversal and header problems in the running console |
| Role probes and a crawl of every page as administrator, operator, restore operator and viewer | every write or admin route refuses roles that may not use it; forms and saved values work in every role and language |
| Appliance image: Trivy over its operating-system packages, a [Lynis](https://cisofy.com/lynis/) hardening audit, [testssl.sh](https://testssl.sh/) on the console port and [Nuclei](https://github.com/projectdiscovery/nuclei) | outdated packages, weak OS settings, TLS problems, known exposures |
| [OpenVAS / Greenbone](https://www.greenbone.net/) and nmap, when the network side of the appliance changes | network-level vulnerabilities and open ports |
| End-to-end QA on real systems | backups and restores of every supported source (Windows, Linux, Proxmox VE, Hyper-V and Hyper-V clusters, VMware ESXi and vCenter, SQL Server, PostgreSQL, MariaDB, NAS, Microsoft 365) to every storage type, roles, LDAP/SSO, API |

Findings are fixed before the release; accepted risks are documented in the release notes.
