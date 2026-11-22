#!/bin/sh
# Tests the logic of the installer scripts that can run without installing
# anything: how postinstall reads an enrollment code from a file.
#   ./packaging/macos/test-scripts.sh
set -eu
cd "$(dirname "$0")"

# The functions under test, taken from postinstall itself (running it would
# install the agent).
log() { :; }
eval "$(sed -n '/^read_code() {$/,/^}$/p' scripts/postinstall)"
command -v read_code >/dev/null || { echo "read_code not found in scripts/postinstall" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/backupzit-pkgtest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
failed=0

# check NAME CONTENT WANT: read_code of a file holding CONTENT prints WANT.
check() {
    printf '%b' "$2" > "$tmp/code"
    got=$(read_code "$tmp/code")
    if [ "$got" != "$3" ]; then
        echo "FAIL $1: got '$got', want '$3'"
        failed=1
    else
        echo "ok   $1"
    fi
}

check "plain code" 'BZ1-eyJzIjoiaHR0cHM6Ly94In0' 'BZ1-eyJzIjoiaHR0cHM6Ly94In0'
check "trailing newline" 'BZ1-abc_DEF-123\n' 'BZ1-abc_DEF-123'
check "CRLF and line breaks from a copy" ' BZ1-abc\r\ndef\r\n' 'BZ1-abcdef'
check "not a code" 'hello' ''
check "prefix only" 'BZ1-' ''
check "damaged (shell characters)" 'BZ1-abc;rm -rf /' ''
check "damaged (quote)" "BZ1-ab'c" ''
check "empty file" '' ''
got=$(read_code "$tmp/missing")
[ -z "$got" ] && echo "ok   missing file" || { echo "FAIL missing file: got '$got'"; failed=1; }

exit "$failed"
