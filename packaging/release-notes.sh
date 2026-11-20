#!/bin/sh
# Changes the notes of a published release: on the GitHub page and in the
# signed manifest that consoles show before an update (and on the testing
# channel when it holds this version). The packages are not touched.
#
#   ./packaging/release-notes.sh 0.43.5 /path/to/release.key notes.md
#
# Editing the notes only on GitHub leaves the old text in the manifest.
#
# The published manifest is signed again only when its current signature
# is from a release key and it is the manifest of VERSION (tools/release
# notes checks both), so changed release files are never signed.
set -eu
VERSION=${1:?usage: release-notes.sh <version> <release.key> <notes file>}
KEY=${2:?path to the release signing key}
NOTES=${3:?notes file}
cd "$(dirname "$0")/.."
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
gh release download "v$VERSION" --repo Max-Zit/backupzit --pattern manifest.json --pattern manifest.json.sig --dir "$TMP"
go run ./tools/release notes -key "$KEY" -dir "$TMP" -notes "$NOTES" -version "$VERSION"
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber "$TMP/manifest.json" "$TMP/manifest.json.sig"
gh release edit "v$VERSION" --repo Max-Zit/backupzit --notes-file "$NOTES"
if gh release view testing --repo Max-Zit/backupzit --json body --jq .body 2>/dev/null | grep -q "BackupZit $VERSION "; then
    gh release upload testing --repo Max-Zit/backupzit --clobber "$TMP/manifest.json" "$TMP/manifest.json.sig"
    echo "testing channel updated too"
fi
echo "notes of v$VERSION updated"
