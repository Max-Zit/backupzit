#!/bin/sh
# Uploads the appliance ISO and VM images of VERSION to the GitHub release
# vVERSION under fixed names, so the documentation can link to
#   https://github.com/Max-Zit/backupzit/releases/latest/download/<name>
#
#   KEY=/path/to/release.key ./publish-images.sh 0.32.0 /root/appliance/out
#
# Files: backupzit-appliance.iso, .ova, .qcow2, backupzit-appliance-vhdx.zip
# (GitHub limits release files to 2 GiB, the VHDX is larger), SHA256SUMS
# and SHA256SUMS.sig.
#
# KEY: the release signing key (as for publish-release.sh). SHA256SUMS.sig
# is its signature of SHA256SUMS, in the same format as manifest.json.sig,
# so users can check that the images are ours (Installation guide). Signing
# runs tools/release, so it needs Go and the source tree around this
# script. An upload from here needs KEY; images are not published with an
# unsigned SHA256SUMS. The key stays on the maintainer's machine: on the
# build host use PREPARE=dir without KEY, copy dir back and sign it there
# before the upload:
#   go run ./tools/release sign-file -key release.key dir/SHA256SUMS
#
# Afterwards prune-images.sh removes the images from releases older than the
# newest 8 (KEEP=n to change it).
# Needs the GitHub CLI (gh), logged in with rights to the repository.
set -eu
VERSION=${1:?usage: publish-images.sh <version> <directory with the build output>}
DIR=$(readlink -f "${2:?directory}")
SRC="backupzit-appliance-$VERSION"
if [ -n "${KEY:-}" ]; then
    KEY=$(readlink -f "$KEY")
    ROOT=$(cd "$(dirname "$0")/../.." && pwd)
    [ -f "$ROOT/tools/release/main.go" ] && command -v go >/dev/null 2>&1 ||
        { echo "KEY needs Go and the BackupZit source tree around this script (tools/release)" >&2; exit 1; }
elif [ -z "${PREPARE:-}" ]; then
    echo "set KEY=/path/to/release.key: the images are published only with a signed SHA256SUMS" >&2
    exit 1
fi
# PREPARE=dir only prepares the files there (upload them elsewhere with
#   gh release upload vVERSION --repo Max-Zit/backupzit dir/*).
if [ -n "${PREPARE:-}" ]; then
    mkdir -p "$PREPARE"
    WORK=$(readlink -f "$PREPARE")
else
    WORK=$(mktemp -d "$DIR/publish.XXXXXX")
    trap 'rm -rf "$WORK"' EXIT
fi
for ext in iso ova qcow2; do
    [ -f "$DIR/$SRC.$ext" ] || { echo "missing $DIR/$SRC.$ext" >&2; exit 1; }
    ln "$DIR/$SRC.$ext" "$WORK/backupzit-appliance.$ext" 2>/dev/null || cp "$DIR/$SRC.$ext" "$WORK/backupzit-appliance.$ext"
done
[ -f "$DIR/$SRC.vhdx" ] || { echo "missing $DIR/$SRC.vhdx" >&2; exit 1; }
ln "$DIR/$SRC.vhdx" "$WORK/backupzit-appliance.vhdx" 2>/dev/null || cp "$DIR/$SRC.vhdx" "$WORK/backupzit-appliance.vhdx"
(cd "$WORK" && zip -q -1 backupzit-appliance-vhdx.zip backupzit-appliance.vhdx && rm backupzit-appliance.vhdx)
(cd "$WORK" && sha256sum backupzit-appliance.iso backupzit-appliance.ova backupzit-appliance.qcow2 backupzit-appliance-vhdx.zip > SHA256SUMS)
rm -f "$WORK/SHA256SUMS.sig"
if [ -n "${KEY:-}" ]; then
    (cd "$ROOT" && go run ./tools/release sign-file -key "$KEY" "$WORK/SHA256SUMS")
fi
for f in "$WORK"/*; do
    size=$(stat -c %s "$f")
    [ "$size" -lt 2147483648 ] || { echo "$f is larger than 2 GiB" >&2; exit 1; }
done
if [ -n "${PREPARE:-}" ]; then
    [ -f "$WORK/SHA256SUMS.sig" ] ||
        echo "SHA256SUMS is not signed yet: before the upload run, where the release key is: go run ./tools/release sign-file -key release.key <copy of $WORK>/SHA256SUMS" >&2
    echo "prepared the images of $VERSION in $WORK"
    exit 0
fi
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber "$WORK"/*
# The asset list shows a label: the VHDX is for generation 1 VMs.
id=$(gh api "repos/Max-Zit/backupzit/releases/tags/v$VERSION" --jq '.assets[] | select(.name == "backupzit-appliance-vhdx.zip") | .id')
[ -n "$id" ] && gh api -X PATCH "repos/Max-Zit/backupzit/releases/assets/$id" -f name=backupzit-appliance-vhdx.zip \
    -f "label=backupzit-appliance-vhdx.zip (Hyper-V, generation 1)" >/dev/null
echo "uploaded the images of $VERSION"
# Keep the images only in the newest releases (KEEP, default 8): about 5 GB
# per release would otherwise pile up on GitHub.
sh "$(dirname "$0")/prune-images.sh"
