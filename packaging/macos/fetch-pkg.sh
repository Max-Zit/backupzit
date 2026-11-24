#!/bin/sh
# Downloads the macOS agent's package for a release from the "macOS package"
# workflow (.github/workflows/macos-pkg.yml, built on a Mac for the tag),
# checks it against the SHA-256 the build computed, and puts it into dist/
# for packaging/linux/build-packages.sh.
#   sh packaging/macos/fetch-pkg.sh 0.59.0
# Needs the GitHub CLI (gh), signed in; waits for the build if it still runs.
set -eu
VERSION=${1:?usage: fetch-pkg.sh <version>}
case $VERSION in *[!0-9A-Za-z.-]*) echo "bad version: $VERSION" >&2; exit 1 ;; esac
cd "$(dirname "$0")/../.."
REPO=${REPO:-Max-Zit/backupzit}
PKG=backupzit-agent_${VERSION}_darwin_universal.pkg

# The newest run of the workflow for the tag.
id=$(gh run list -R "$REPO" --workflow macos-pkg.yml --branch "v$VERSION" --limit 1 --json databaseId --jq '.[0].databaseId // empty')
[ -n "$id" ] || { echo "no macOS package build for v$VERSION: push the tag, or start it with: gh workflow run macos-pkg.yml -R $REPO -f version=$VERSION" >&2; exit 1; }
gh run watch "$id" -R "$REPO" --exit-status >/dev/null || { echo "the macOS package build $id failed: gh run view $id -R $REPO --log-failed" >&2; exit 1; }

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
gh run download "$id" -R "$REPO" -n macos-pkg -D "$tmp"
[ -f "$tmp/$PKG" ] && [ -f "$tmp/SHA256SUMS" ] || { echo "the build $id has no $PKG" >&2; exit 1; }
want=$(awk -v f="$PKG" '$2 == f || $2 == "*"f { print $1 }' "$tmp/SHA256SUMS")
got=$(sha256sum "$tmp/$PKG" 2>/dev/null | awk '{print $1}') || got=$(shasum -a 256 "$tmp/$PKG" | awk '{print $1}')
[ -n "$want" ] && [ "$want" = "$got" ] || { echo "$PKG: SHA-256 $got does not match the build's $want" >&2; exit 1; }
mkdir -p dist
cp "$tmp/$PKG" "dist/$PKG"
echo "dist/$PKG ($got, build $id)"
