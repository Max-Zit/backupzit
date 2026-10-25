#!/bin/sh
# Makes an approved release candidate the official release: the same
# signed files, now the newest release that consoles with the default
# update source install.
#
#   ./packaging/promote-release.sh 0.43.0
#
# Afterwards merge the tested commit into main (git merge --ff-only vVERSION).
set -eu
VERSION=${1:?usage: promote-release.sh <version>}
gh release view "v$VERSION" --repo Max-Zit/backupzit --json isPrerelease --jq .isPrerelease | grep -q true ||
    { echo "v$VERSION is not a release candidate" >&2; exit 1; }
gh release edit "v$VERSION" --repo Max-Zit/backupzit --prerelease=false --latest
echo "v$VERSION is the official release"
