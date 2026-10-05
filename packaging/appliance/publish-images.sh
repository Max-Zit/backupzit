#!/bin/sh
# Uploads the appliance ISO and VM images of VERSION to the GitHub release
# vVERSION under fixed names, so the documentation can link to
#   https://github.com/Max-Zit/backupzit/releases/latest/download/<name>
#
#   ./publish-images.sh 0.32.0 /root/appliance/out
#
# Files: backupzit-appliance.iso, .ova, .qcow2, backupzit-appliance-vhdx.zip
# (GitHub limits release files to 2 GiB, the VHDX is larger) and SHA256SUMS.
# Needs the GitHub CLI (gh), logged in with rights to the repository.
set -eu
VERSION=${1:?usage: publish-images.sh <version> <directory with the build output>}
DIR=$(readlink -f "${2:?directory}")
SRC="backupzit-appliance-$VERSION"
# PREPARE=dir only prepares the files there (upload them elsewhere with
#   gh release upload vVERSION --repo Max-Zit/backupzit dir/*).
if [ -n "${PREPARE:-}" ]; then
    WORK=$PREPARE
    mkdir -p "$WORK"
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
for f in "$WORK"/*; do
    size=$(stat -c %s "$f")
    [ "$size" -lt 2147483648 ] || { echo "$f is larger than 2 GiB" >&2; exit 1; }
done
if [ -n "${PREPARE:-}" ]; then
    echo "prepared the images of $VERSION in $WORK"
    exit 0
fi
gh release upload "v$VERSION" --repo Max-Zit/backupzit --clobber "$WORK"/*
echo "uploaded the images of $VERSION"
