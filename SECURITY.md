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
- The code is checked with `govulncheck`, `gosec`, `staticcheck`, Semgrep, Trivy and Gitleaks, and the web console with authenticated OWASP ZAP and OpenVAS scans.
