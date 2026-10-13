# Contributing to BackupZit

Thank you for helping! BackupZit is free software under the GNU AGPLv3.

## Questions, bugs and ideas

- **Bugs:** open an [issue](https://github.com/Max-Zit/backupzit/issues) with the BackupZit version, the operating system of the console and the agent, what you did, what happened and what you expected. Logs help: *Agents → Get log* and *Settings → Diagnostics* in the console.
- **Ideas:** open an issue that describes the problem you want to solve.
- **Security problems:** do not open a public issue; see [SECURITY.md](SECURITY.md).

## Code

1. Go (the version in `go.mod`) is all you need: `go build ./cmd/...` and `go test ./...` (the tests start an embedded PostgreSQL).
2. Keep changes focused, with tests for new behavior.
3. Functional changes update the built-in user guide (`internal/server/templates/doc_*.html` and the translations in `internal/server/templates/docs/<lang>/`) and the UI texts in all 14 catalogs (`internal/server/i18n/`); `go run ./tools/i18n missing sr` lists missing texts.
4. New dependencies must have a license compatible with the AGPLv3; `go run ./tools/licenses` checks it.

By contributing you agree that your contribution is licensed under the GNU AGPLv3.
