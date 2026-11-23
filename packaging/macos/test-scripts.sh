#!/bin/sh
# Tests the logic of the installer scripts that can run without installing
# anything: how postinstall reads an enrollment code from a file, and when it
# trusts the backupzit-enroll.txt next to the package.
#   ./packaging/macos/test-scripts.sh
set -eu
cd "$(dirname "$0")"

# The functions under test, taken from postinstall itself (running it would
# install the agent).
log() { :; }
for fn in read_code read_code_from is_admin trusted_side_file side_code; do
    eval "$(sed -n '/^'"$fn"'() {$/,/^}$/p' scripts/postinstall)"
    command -v "$fn" >/dev/null || { echo "$fn not found in scripts/postinstall" >&2; exit 1; }
done

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

# The ownership tests replace stat and the admin lookup with stubs: the
# files of a test belong to whoever runs it, and only root could give them
# to another user. STAT_FILE_UID / STAT_LINKS / STAT_DIR_UID / STAT_DIR_MODE
# hold what stat answers for the side file and its folder; user 501 is an
# administrator, 502 a standard user.
stat() {
    case $2:${3:-} in
        %d:%i:) echo "${STAT_DEV:-1}:7" ;;
        %d:%i:"$tmp/pkg/backupzit-enroll.txt") echo "${STAT_DEV:-1}:${STAT_INO:-7}" ;;
        %d:"$tmp/etc") echo 1 ;;
        %Lp:"$tmp/pkg/backupzit-enroll.txt") echo "${STAT_FILE_MODE:-644}" ;;
        %u:"$tmp/pkg/backupzit-enroll.txt") echo "$STAT_FILE_UID" ;;
        %l:"$tmp/pkg/backupzit-enroll.txt") echo "$STAT_LINKS" ;;
        %u:"$tmp/pkg") echo "$STAT_DIR_UID" ;;
        %Mp%Lp:"$tmp/pkg") echo "$STAT_DIR_MODE" ;;
        *) return 1 ;;
    esac
}
id() { case $2 in 501) echo admin1 ;; 502) echo bob ;; *) return 1 ;; esac; }
dseditgroup() { [ "$4" = admin1 ] && [ "$5" = admin ]; }
ID_CMD=id DSEDITGROUP=dseditgroup

code='BZ1-abc_DEF-123'
mkdir "$tmp/pkg" "$tmp/etc"
: > "$tmp/pkg/BackupZit.pkg"
AGENT_CFG=$tmp/etc/agent.json
MDM_CODE=/nonexistent/enroll-code
SYSTEM_DATA=$tmp/etc

# side FILE_UID LINKS DIR_UID DIR_MODE NAME WANT: side_code prints WANT when
# stat answers these owners, link count and folder mode.
side() {
    STAT_FILE_UID=$1 STAT_LINKS=$2 STAT_DIR_UID=$3 STAT_DIR_MODE=$4
    got=$(side_code "$tmp/pkg/BackupZit.pkg#inner.pkg")
    if [ "$got" != "$6" ]; then
        echo "FAIL side file, $5: got '$got', want '$6'"
        failed=1
    else
        echo "ok   side file, $5"
    fi
}

printf '%s\n' "$code" > "$tmp/pkg/backupzit-enroll.txt"
side 0 1 0 0755       "all owned by root" "$code"
side 501 1 501 0755   "all owned by an administrator" "$code"
side 501 1 0 0755     "administrator's file in root's folder" "$code"
side 502 1 502 0755   "all owned by a standard user (who may own the package)" ''
side 502 1 0 0755     "file of a standard user" ''
side 501 1 502 0755   "folder of a standard user" ''
side 501 2 501 0755   "file with a second name (hard link)" ''
side 0 2 0 0755       "root's file with a second name" ''
side 501 1 501 0775   "folder writable by its group" ''
side 501 1 0 0777     "folder writable by everyone" ''
side 501 1 0 1777     "sticky folder (/Users/Shared), administrator's file" "$code"
side 502 1 0 1777     "sticky folder, file of a standard user" ''
side '' 1 501 0755    "owner unreadable" ''
side 501 '' 501 0755  "link count unreadable" ''
side 501 1 501 'x'    "mode unreadable" ''
side 501 1 501 0758   "mode not octal" ''
# A disk image or USB stick may ignore ownership: its files claim any owner.
STAT_DEV=2; side 0 1 0 0755 "on a disk image or USB stick" ''; unset STAT_DEV
STAT_FILE_MODE=666; side 501 1 501 0755 "file others may change" ''; unset STAT_FILE_MODE
STAT_FILE_MODE=0x; side 501 1 501 0755 "file mode unreadable" ''; unset STAT_FILE_MODE
STAT_INO=8; side 501 1 501 0755 "file swapped while it was checked" ''; unset STAT_INO

: > "$AGENT_CFG"
side 501 1 501 0755   "Mac already enrolled" ''
rm -f "$AGENT_CFG"

rm -f "$tmp/pkg/backupzit-enroll.txt"
printf '%s\n' "$code" > "$tmp/elsewhere"
ln -s "$tmp/elsewhere" "$tmp/pkg/backupzit-enroll.txt"
side 501 1 501 0755   "symbolic link" ''
rm -f "$tmp/pkg/backupzit-enroll.txt"
side 501 1 501 0755   "no side file" ''

# The same rule with the real stat, on files of the user running the test
# (taken as an administrator here): what matters is that stat reports the
# sticky bit and the link count.
unset -f stat id dseditgroup
is_admin() { [ "$1" = "$(command id -u)" ] || [ "$1" = 0 ]; }
mkdir "$tmp/real"
: > "$tmp/real/BackupZit.pkg"
printf '%s\n' "$code" > "$tmp/real/backupzit-enroll.txt"
# real MODE NAME WANT: side_code prints WANT with the folder in MODE.
real() {
    chmod "$1" "$tmp/real"
    got=$(side_code "$tmp/real/BackupZit.pkg")
    if [ "$got" != "$3" ]; then
        echo "FAIL real stat, $2: got '$got', want '$3'"
        failed=1
    else
        echo "ok   real stat, $2"
    fi
}
SYSTEM_DATA=$tmp
real 0755 "private folder" "$code"
SYSTEM_DATA=/dev
real 0755 "on another volume" ''
SYSTEM_DATA=$tmp
chmod 0666 "$tmp/real/backupzit-enroll.txt"
real 0755 "file writable by everyone" ''
chmod 0644 "$tmp/real/backupzit-enroll.txt"
real 1777 "sticky folder" "$code"
real 0777 "folder writable by everyone" ''
ln "$tmp/real/backupzit-enroll.txt" "$tmp/real/second-name"
real 0755 "file with a second name" ''
chmod 0700 "$tmp/real"

exit "$failed"
