#!/bin/sh
# Tests the part of backupzit-appliance-boot that runs without an appliance:
# which web console address the login screen shows, read from the console's
# configuration (/etc/backupzit/server.env).
#   ./packaging/appliance/test-boot.sh
set -eu
cd "$(dirname "$0")"

# The functions under test, taken from the script itself (running it would
# change the system).
for fn in env_value console_url; do
    eval "$(sed -n '/^'"$fn"'() {$/,/^}$/p' backupzit-appliance-boot)"
    command -v "$fn" >/dev/null || { echo "$fn not found in backupzit-appliance-boot" >&2; exit 1; }
done
grep -q 'url=$(console_url /etc/backupzit/server.env)' backupzit-appliance-boot ||
    { echo "write_issue does not use console_url" >&2; exit 1; }

tmp=$(mktemp -d "${TMPDIR:-/tmp}/backupzit-boottest.XXXXXX")
trap 'rm -rf "$tmp"' EXIT
failed=0

# check NAME CONTENT WANT: console_url for a server.env holding CONTENT
# (printf %b: \n and \r are line ends) prints WANT.
check() {
    printf '%b' "$2" > "$tmp/server.env"
    got=$(console_url "$tmp/server.env")
    if [ "$got" != "$3" ]; then
        echo "FAIL $1: got '$got', want '$3'"
        failed=1
    else
        echo "ok   $1"
    fi
}

# \4 stays as it is: the login prompt fills in the IPv4 address.
cp ../linux/server.env "$tmp/server.env"
got=$(console_url "$tmp/server.env")
[ "$got" = 'https://\4:8443' ] && echo "ok   server.env of the package" || { echo "FAIL server.env of the package: got '$got'"; failed=1; }
got=$(console_url "$tmp/missing")
[ "$got" = 'https://\4:8443' ] && echo "ok   no server.env" || { echo "FAIL no server.env: got '$got'"; failed=1; }

check "empty file" '' 'https://\4:8443'
check "no listen line" 'BACKUPZIT_DB=postgres://x\n' 'https://\4:8443'
check "port 3000" 'BACKUPZIT_LISTEN=:3000\n' 'https://\4:3000'
check "all IPv4 addresses" 'BACKUPZIT_LISTEN=0.0.0.0:3000\n' 'https://\4:3000'
check "IPv6" 'BACKUPZIT_LISTEN=[::]:9443\n' 'https://\4:9443'
check "port 443 is not shown" 'BACKUPZIT_LISTEN=:443\n' 'https://\4'
check "double quotes" 'BACKUPZIT_LISTEN=":3000"\n' 'https://\4:3000'
check "single quotes" "BACKUPZIT_LISTEN=':3000'\n" 'https://\4:3000'
check "CRLF line ends" 'BACKUPZIT_DB=x\r\nBACKUPZIT_LISTEN=:3000\r\n' 'https://\4:3000'
check "blanks around" '  BACKUPZIT_LISTEN = :3000  \n' 'https://\4:3000'
check "the last line counts" 'BACKUPZIT_LISTEN=:3000\nBACKUPZIT_LISTEN=:4000\n' 'https://\4:4000'
check "commented out" '#BACKUPZIT_LISTEN=:3000\n' 'https://\4:8443'
check "another setting with the same start" 'BACKUPZIT_LISTEN_OLD=:3000\n' 'https://\4:8443'
check "empty value (console default)" 'BACKUPZIT_LISTEN=:3000\nBACKUPZIT_LISTEN=\n' 'https://\4:8443'
check "no colon" 'BACKUPZIT_LISTEN=3000\n' 'https://\4:8443'
check "port name" 'BACKUPZIT_LISTEN=:https\n' 'https://\4:8443'
check "port 0" 'BACKUPZIT_LISTEN=:0\n' 'https://\4:8443'
check "leading zero" 'BACKUPZIT_LISTEN=:03000\n' 'https://\4:3000'
check "port too big" 'BACKUPZIT_LISTEN=:70000\n' 'https://\4:8443'
check "port far too big" 'BACKUPZIT_LISTEN=:99999999999999999999\n' 'https://\4:8443'
check "public URL" 'BACKUPZIT_LISTEN=:3000\nBACKUPZIT_PUBLIC_URL=https://backup.example.com:3000\n' 'https://backup.example.com:3000'
check "public URL in quotes" 'BACKUPZIT_PUBLIC_URL="https://backup.example.com"\n' 'https://backup.example.com'
check "public URL commented out" '#BACKUPZIT_PUBLIC_URL=https://backup.example.com\nBACKUPZIT_LISTEN=:3000\n' 'https://\4:3000'
check "public URL empty" 'BACKUPZIT_PUBLIC_URL=\nBACKUPZIT_LISTEN=:3000\n' 'https://\4:3000'
check "login prompt escapes in the public URL" 'BACKUPZIT_PUBLIC_URL=https://backup\\n.example.com\n' 'https://backupn.example.com'

exit "$failed"
