# Security policy

## Supported versions

Security fixes are released for the newest BackupZit version. Consoles check for updates and install signed releases from *Settings → Updates*; agents are updated from the *Agents* page.

## Reporting a vulnerability

Please report security problems privately, not in a public issue:

1. Open the [Security tab](https://github.com/Max-Zit/backupzit/security) of this repository.
2. Click **Report a vulnerability** and describe the problem, the affected version and how to reproduce it.

We confirm reports within a few working days, fix confirmed problems in a new release and credit the reporter in the release notes unless they prefer otherwise.

## How releases are protected

- Every release is signed with the BackupZit release key; consoles and agents verify the signature and checksums before installing an update.
- Release packages are built from tagged commits of this repository; the appliance and recovery images carry a written source offer (`SOURCE-CODE.txt`).

## How BackupZit is tested

**On every change (GitHub Actions, results in the [Security tab](https://github.com/Max-Zit/backupzit/security)):**

| Check | What it finds |
|---|---|
| [CI](https://github.com/Max-Zit/backupzit/actions/workflows/ci.yml): `go vet` and the full test suite on Linux, build for Windows | regressions; the tests run the console against an embedded PostgreSQL and in-process SFTP, S3 and vSphere simulators |
| [CodeQL](https://codeql.github.com/) (Go, JavaScript, workflows) | injection, path traversal, unsafe redirects and other data-flow bugs |
| [govulncheck](https://go.dev/doc/security/vuln/) | known vulnerabilities in Go and in the dependencies the code actually calls |
| [staticcheck](https://staticcheck.dev/) | bugs and dead code |
| [Dependabot](https://docs.github.com/code-security/dependabot) | vulnerable or outdated Go modules and actions |
| Secret scanning with push protection | credentials committed by mistake |
| [OpenSSF Scorecard](https://scorecard.dev/viewer/?uri=github.com/Max-Zit/backupzit) | supply-chain practices of the repository |

**Before releases (manually, in a test lab):**

| Check | What it finds |
|---|---|
| [gosec](https://github.com/securego/gosec) | insecure Go patterns: command execution, file permissions, weak cryptography |
| [Semgrep](https://semgrep.dev/) (Go, JavaScript, secrets, OWASP Top 10 rules) | injection, XSS, insecure cookies, open redirects |
| [Trivy](https://trivy.dev/) | vulnerable dependencies, secrets and misconfigurations |
| [Gitleaks](https://gitleaks.io/) | secrets in the whole git history |
| [OWASP ZAP](https://www.zaproxy.org/) full scan, signed in as an administrator | XSS, SQL injection, CSRF, path traversal and header problems in the running console |
| [OpenVAS / Greenbone](https://www.greenbone.net/) and nmap | network-level vulnerabilities, open ports and TLS settings of the appliance |
| End-to-end QA on real systems | backups and restores of every supported source (Windows, Linux, Proxmox VE, Hyper-V, VMware ESXi and vCenter, SQL Server, PostgreSQL, MariaDB, NAS, Microsoft 365) to every storage type, roles, LDAP/SSO, API |

Findings are fixed before the release; accepted risks are documented in the release notes.
