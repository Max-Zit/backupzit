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
#
# The files outside the signed manifest (install.sh, build-winpe.ps1,
# SOURCE-CODE.txt and the Linux recovery ISO) are listed with their SHA-256
# in release-files.sha256, signed with the same key (release-files.sha256.sig).
# FILES_ONLY=1 uploads only these files and a new signed list to the
# existing release vVERSION, e.g. when the recovery ISO is built later (run
# it from the release's commit).
set -eu
VERSION=${1:?usage: publish-release.sh <version> <release.key> [notes file]}
KEY=${2:?path to the release signing key}
NOTES=${3:-}
cd "$(dirname "$0")/.."
OUT="dist/release-$VERSION"
FLAGS=""
[ -n "${CANDIDATE:-}" ] && FLAGS="--prerelease --latest=false"
if [ -z "${FILES_ONLY:-}" ]; then
    rm -rf "$OUT" && mkdir -p "$OUT"
    cp dist/*"$VERSION"* "$OUT"/ 2>/dev/null || true
    rm -rf "$OUT/release-$VERSION" "$OUT"/*.iso  # the recovery ISO goes up once, under its fixed name (below)
    ls "$OUT" | grep -q "backupzit-server_${VERSION}_" || { echo "no server package for $VERSION in dist/" >&2; exit 1; }
fi

# The signed list of the other files, made before anything is uploaded so
# that a wrong key stops the release at the start. (A dot directory:
# dist/*VERSION* above does not pick it up.)
FILES="packaging/install.sh packaging/winpe/build-winpe.ps1 packaging/appliance/SOURCE-CODE.txt"
ISO="dist/backupzit-recovery-linux-$VERSION.iso"
if [ -f "$ISO" ]; then
    cp "$ISO" dist/backupzit-recovery-linux.iso
    FILES="$FILES dist/backupzit-recovery-linux.iso"
fi
EXTRA="dist/.release-files-$VERSION"
rm -rf "$EXTRA" && mkdir -p "$EXTRA"
trap 'rm -rf "$EXTRA"' EXIT
: > "$EXTRA/release-files.sha256"
for f in $FILES; do
    sum=$(sha256sum < "$f")
    printf '%s  %s\n' "${sum%% *}" "${f##*/}" >> "$EXTRA/release-files.sha256"
done
go run ./tools/release sign-file -key "$KEY" "$EXTRA/release-files.sha256"

if [ -z "${FILES_ONLY:-}" ]; then
    if [ -n "$NOTES" ]; then
        go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT" -notes "$NOTES"
        gh release create "v$VERSION" --repo Max-Zit/backupzit $FLAGS --title "BackupZit $VERSION" --notes-file "$NOTES" "$OUT"/*
    else
        go run ./tools/release sign -key "$KEY" -version "$VERSION" -dir "$OUT"
        gh release create "v$VERSION" --repo Max-Zit/backupzit $FLAGS --title "BackupZit $VERSION" --notes "BackupZit $VERSION" "$OUT"/*
    fi
fi
# The install script for existing servers (it checks the signature of the
# release itself before it installs anything).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/install.sh
# The script that builds the Windows recovery ISO on the user's PC (Windows
# PE cannot be redistributed), and the Linux recovery ISO when built
# (packaging/linux-recovery/build-iso.sh, on a Debian/Proxmox host).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/winpe/build-winpe.ps1
# Where to get the source code of the GPL software in the appliance images
# and the recovery ISO (written offer).
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber packaging/appliance/SOURCE-CODE.txt
if [ -f "$ISO" ]; then
    gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber dist/backupzit-recovery-linux.iso
else
    echo "warning: no $ISO; build it, then upload it with FILES_ONLY=1 $0 $VERSION <release.key> before promote-release.sh (it refuses without it)" >&2
fi
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber "$EXTRA/release-files.sha256" "$EXTRA/release-files.sha256.sig"
if [ -n "${FILES_ONLY:-}" ]; then
    echo "uploaded the files of v$VERSION and their signed list"
    exit 0
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
