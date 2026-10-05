#!/bin/sh
# Publishes a release on GitHub: signs the packages of VERSION in dist/ and
# uploads them with manifest.json and manifest.json.sig as release assets.
# Consoles find the newest release at
#   https://github.com/Max-Zit/backupzit/releases/latest/download
#
#   ./packaging/publish-release.sh 0.31.0 /path/to/release.key notes.txt
#
# Build first: packaging/windows/build-msi.ps1 [-Legacy] (on Windows) and
# packaging/linux/build-packages.sh VERSION. Needs the GitHub CLI (gh),
# logged in with rights to the repository, and the tag vVERSION pushed.
set -eu
VERSION=${1:?usage: publish-release.sh <version> <release.key> [notes file]}
KEY=${2:?path to the release signing key}
NOTES=${3:-}
cd "$(dirname "$0")/.."
OUT="dist/release-$VERSION"
rm -rf "$OUT" && mkdir -p "$OUT"
cp dist/*"$VERSION"* "$OUT"/ 2>/dev/null || true
rm -rf "$OUT/release-$VERSION"
ls "$OUT" | grep -q "backupzit-server_${VERSION}_" || { echo "no server package for $VERSION in dist/" >&2; exit 1; }
if [ -n "$NOTES" ]; then
    go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT" -notes "$NOTES"
    gh release create "v$VERSION" --repo Max-Zit/backupzit --title "BackupZit $VERSION" --notes-file "$NOTES" "$OUT"/*
else
    go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT"
    gh release create "v$VERSION" --repo Max-Zit/backupzit --title "BackupZit $VERSION" --notes "BackupZit $VERSION" "$OUT"/*
fi
# The install script for existing servers (not part of the signed manifest:
# it checks the signature of the release itself).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/install.sh
echo "published v$VERSION"
