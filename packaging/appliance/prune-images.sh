#!/bin/sh
# Keeps the appliance images (ISO, OVA, QCOW2, VHDX zip) only in the newest
# KEEP releases on GitHub (default 5) and deletes them from older releases;
# the programs, installers and the recovery ISO of every release stay.
# The documentation links to releases/latest/download, so nothing points at
# the images of an old release. publish-images.sh runs it after an upload.
#
#   ./prune-images.sh            delete
#   DRY_RUN=1 ./prune-images.sh  only list what would be deleted
#   KEEP=8 ./prune-images.sh     keep the images of the newest 8 releases
#
# Needs the GitHub CLI (gh), logged in with rights to the repository.
set -eu
REPO=Max-Zit/backupzit
KEEP=${KEEP:-5}
IMAGES='backupzit-appliance.iso backupzit-appliance.ova backupzit-appliance.qcow2 backupzit-appliance-vhdx.zip'
freed=0
n=0
# Newest first (by creation time, drafts excluded).
gh api --paginate "repos/$REPO/releases?per_page=100" \
    --jq '[.[] | select(.draft | not)] | sort_by(.created_at) | reverse | .[] | .tag_name' > /tmp/bz-releases.$$
trap 'rm -f /tmp/bz-releases.$$' EXIT
while read -r tag; do
    n=$((n + 1))
    [ "$n" -le "$KEEP" ] && continue
    for name in $IMAGES; do
        line=$(gh api "repos/$REPO/releases/tags/$tag" --jq ".assets[] | select(.name == \"$name\") | \"\(.id) \(.size)\"")
        [ -n "$line" ] || continue
        id=${line% *}
        size=${line#* }
        if [ -n "${DRY_RUN:-}" ]; then
            echo "would delete $tag/$name ($((size / 1048576)) MB)"
        else
            gh api -X DELETE "repos/$REPO/releases/assets/$id" >/dev/null
            echo "deleted $tag/$name ($((size / 1048576)) MB)"
        fi
        freed=$((freed + size / 1048576))
    done
done < /tmp/bz-releases.$$
echo "images kept in the newest $KEEP releases; $freed MB ${DRY_RUN:+would be }freed"
