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
#
# CANDIDATE=1 publishes a release candidate: a pre-release that consoles
# with the default source do not see, also copied to the release "testing"
# (update source https://github.com/Max-Zit/backupzit/releases/download/testing).
# packaging/promote-release.sh VERSION later makes it the official release.
set -eu
VERSION=${1:?usage: publish-release.sh <version> <release.key> [notes file]}
KEY=${2:?path to the release signing key}
NOTES=${3:-}
cd "$(dirname "$0")/.."
OUT="dist/release-$VERSION"
FLAGS=""
[ -n "${CANDIDATE:-}" ] && FLAGS="--prerelease --latest=false"
rm -rf "$OUT" && mkdir -p "$OUT"
cp dist/*"$VERSION"* "$OUT"/ 2>/dev/null || true
rm -rf "$OUT/release-$VERSION" "$OUT"/*.iso  # the recovery ISO goes up once, under its fixed name (below)
ls "$OUT" | grep -q "backupzit-server_${VERSION}_" || { echo "no server package for $VERSION in dist/" >&2; exit 1; }
if [ -n "$NOTES" ]; then
    go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT" -notes "$NOTES"
    gh release create "v$VERSION" --repo Max-Zit/backupzit $FLAGS --title "BackupZit $VERSION" --notes-file "$NOTES" "$OUT"/*
else
    go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT"
    gh release create "v$VERSION" --repo Max-Zit/backupzit $FLAGS --title "BackupZit $VERSION" --notes "BackupZit $VERSION" "$OUT"/*
fi
# The install script for existing servers (not part of the signed manifest:
# it checks the signature of the release itself).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/install.sh
# The script that builds the Windows recovery ISO on the user's PC (Windows
# PE cannot be redistributed), and the Linux recovery ISO when built
# (packaging/linux-recovery/build-iso.sh, on a Debian/Proxmox host).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/winpe/build-winpe.ps1
# Where to get the source code of the GPL software in the appliance images
# and the recovery ISO (written offer).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/appliance/SOURCE-CODE.txt
if [ -f "dist/backupzit-recovery-linux-$VERSION.iso" ]; then
    cp "dist/backupzit-recovery-linux-$VERSION.iso" "dist/backupzit-recovery-linux.iso"
    gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber dist/backupzit-recovery-linux.iso
fi
if [ -n "${CANDIDATE:-}" ]; then
    # The testing channel holds exactly the candidate's signed files.
    if ! gh release view testing --repo Max-Zit/backupzit >/dev/null 2>&1; then
        gh release create testing --repo Max-Zit/backupzit --prerelease --latest=false --title "Testing channel" \
            --notes "Release candidates for test consoles. Update source: https://github.com/Max-Zit/backupzit/releases/download/testing"
    fi
    for a in $(gh release view testing --repo Max-Zit/backupzit --json assets --jq '.assets[].name'); do
        gh release delete-asset testing "$a" --repo Max-Zit/backupzit --yes
    done
    gh release upload testing --repo Max-Zit/backupzit --clobber "$OUT"/*
    gh release edit testing --repo Max-Zit/backupzit --notes "Current candidate: BackupZit $VERSION (https://github.com/Max-Zit/backupzit/releases/tag/v$VERSION). For test consoles only. Update source: https://github.com/Max-Zit/backupzit/releases/download/testing"
    echo "published candidate v$VERSION (also on the testing channel)"
    exit 0
fi
echo "published v$VERSION"
