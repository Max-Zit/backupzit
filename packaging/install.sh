#!/bin/sh
# Installs the BackupZit console on an existing Linux server: PostgreSQL,
# the newest signed BackupZit release, a database with a random password
# and the configuration. Run as root on Debian 12+, Ubuntu 22.04+,
# AlmaLinux / Rocky Linux 9+:
#
#   curl -fsSL https://github.com/Max-Zit/backupzit/releases/latest/download/install.sh | sudo sh
#
# Options (sh -s -- OPTIONS when piped):
#   --version X        install release X instead of the newest one
#   --db URL           use an existing PostgreSQL database (postgres://…)
#                      instead of installing PostgreSQL locally
#   --hosts LIST       names and addresses for the console's certificate
#                      (comma separated; default: this server's address and name)
#   --no-auto-updates  do not switch on automatic security updates
#   --source URL       release location (default: the official GitHub releases)
#
# Running it again on an installed console updates BackupZit and keeps the
# configuration and data.
set -eu

SOURCE=https://github.com/Max-Zit/backupzit/releases
VERSION=""
DB_URL=""
HOSTS=""
AUTO_UPDATES=1
# Release signing keys (base64 Ed25519), as in internal/update.TrustedKeys.
KEYS="+ad/LUTVtGXwVv4qyca7QgmSErAbXTFmje1iWyFlmGo="

say() { printf '\033[1m==> %s\033[0m\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --version) VERSION=${2:?}; shift 2 ;;
        --db) DB_URL=${2:?}; shift 2 ;;
        --hosts) HOSTS=${2:?}; shift 2 ;;
        --no-auto-updates) AUTO_UPDATES=0; shift ;;
        --source) SOURCE=${2:?}; shift 2 ;;
        -h|--help) sed -n '2,22p' "$0" 2>/dev/null || true; exit 0 ;;
        *) die "unknown option $1" ;;
    esac
done

[ "$(id -u)" = 0 ] || die "run as root (sudo sh)"
[ "$(uname -m)" = x86_64 ] || die "BackupZit packages are built for x86_64 (amd64)"
[ -r /etc/os-release ] || die "unknown Linux distribution"
OS_IDS=$(. /etc/os-release; echo "${ID:-} ${ID_LIKE:-}")
OS_NAME=$(. /etc/os-release; echo "${PRETTY_NAME:-$ID}")
case " $OS_IDS " in
    *" debian "*|*" ubuntu "*) PM=apt; FORMAT=deb ;;
    *" rhel "*|*" fedora "*|*" centos "*|*" almalinux "*|*" rocky "*) PM=dnf; FORMAT=rpm ;;
    *) die "unsupported distribution $OS_NAME; use Debian, Ubuntu, AlmaLinux or Rocky Linux" ;;
esac

say "Installing required packages"
if [ $PM = apt ]; then
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -q
    PKGS="curl openssl ca-certificates"
    [ -z "$DB_URL" ] && PKGS="$PKGS postgresql"
    [ $AUTO_UPDATES = 1 ] && PKGS="$PKGS unattended-upgrades"
    # shellcheck disable=SC2086
    apt-get install -y -q $PKGS
else
    PKGS="curl openssl ca-certificates"
    [ -z "$DB_URL" ] && PKGS="$PKGS postgresql-server"
    [ $AUTO_UPDATES = 1 ] && PKGS="$PKGS dnf-automatic"
    # shellcheck disable=SC2086
    dnf install -y -q $PKGS
fi
openssl version | grep -q '^OpenSSL [3-9]' || die "OpenSSL 3 or newer is needed to check the release signature"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
if [ -n "$VERSION" ]; then
    BASE="$SOURCE/download/v$VERSION"
else
    BASE="$SOURCE/latest/download"
fi
case "$SOURCE" in https://*|file://*) ;; *) BASE="$SOURCE" ;; esac

say "Downloading the release from $BASE"
curl -fsSL -o "$WORK/manifest.json" "$BASE/manifest.json" || die "no release found at $BASE"
curl -fsSL -o "$WORK/manifest.json.sig" "$BASE/manifest.json.sig" || die "the release has no signature"

# Check the Ed25519 signature with each trusted key (SubjectPublicKeyInfo
# prefix for Ed25519 + the raw key, both in base64).
base64 -d "$WORK/manifest.json.sig" > "$WORK/sig.bin" 2>/dev/null || die "invalid release signature"
OK=0
for k in $KEYS; do
    printf -- '-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEA%s\n-----END PUBLIC KEY-----\n' "$k" > "$WORK/key.pem"
    if openssl pkeyutl -verify -pubin -inkey "$WORK/key.pem" -rawin -in "$WORK/manifest.json" -sigfile "$WORK/sig.bin" >/dev/null 2>&1; then
        OK=1
        break
    fi
done
[ $OK = 1 ] || die "the release is not signed with a BackupZit release key"

# The server package of this distribution's format, with its checksum.
eval "$(awk -v fmt="$FORMAT" '
    /^[[:space:]]*\{/ { name = kind = format = sum = "" }
    /"version":/ && !ver { ver = $2; gsub(/[",]/, "", ver) }
    /"name":/ { name = $2; gsub(/[",]/, "", name) }
    /"kind":/ { kind = $2; gsub(/[",]/, "", kind) }
    /"format":/ { format = $2; gsub(/[",]/, "", format) }
    /"sha256":/ { sum = $2; gsub(/[",]/, "", sum) }
    /^[[:space:]]*\}/ { if (kind == "server" && format == fmt) { pkg = name; pkgsum = sum } }
    END { printf "REL_VERSION=%s\nPKG=%s\nPKG_SHA=%s\n", ver, pkg, pkgsum }
' "$WORK/manifest.json")"
case "$PKG" in ""|*/*|*..*) die "the release has no $FORMAT server package" ;; esac
say "BackupZit $REL_VERSION: $PKG"
curl -fsSL -o "$WORK/$PKG" "$BASE/$PKG" || die "download of $PKG failed"
echo "$PKG_SHA  $WORK/$PKG" | sha256sum -c - >/dev/null || die "checksum of $PKG does not match the signed release"

FRESH=1
[ -f /etc/backupzit/server.env ] && grep -q '^BACKUPZIT_DB=postgres://' /etc/backupzit/server.env && FRESH=0

say "Installing BackupZit $REL_VERSION"
if [ $PM = apt ]; then
    apt-get install -y -q "$WORK/$PKG"
else
    dnf install -y -q "$WORK/$PKG"
fi

if [ $FRESH = 1 ]; then
    ENV=/etc/backupzit/server.env
    if [ -z "$DB_URL" ]; then
        say "Setting up PostgreSQL"
        if [ $PM = dnf ] && [ ! -f /var/lib/pgsql/data/PG_VERSION ]; then
            postgresql-setup --initdb
            # Local password logins over TCP (the default is ident).
            sed -i 's/^\(host[[:space:]].*127\.0\.0\.1\/32[[:space:]]*\)ident/\1scram-sha-256/; s/^\(host[[:space:]].*::1\/128[[:space:]]*\)ident/\1scram-sha-256/' /var/lib/pgsql/data/pg_hba.conf
        fi
        systemctl enable --now postgresql
        DB_PASS=$(head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 24)
        su postgres -s /bin/sh -c "cd /; psql -qc \"SET password_encryption = 'scram-sha-256'; CREATE USER backupzit PASSWORD '$DB_PASS';\"" 2>/dev/null ||
            su postgres -s /bin/sh -c "cd /; psql -qc \"SET password_encryption = 'scram-sha-256'; ALTER USER backupzit PASSWORD '$DB_PASS';\""
        su postgres -s /bin/sh -c "cd /; createdb -O backupzit backupzit" 2>/dev/null || true
        DB_URL="postgres://backupzit:$DB_PASS@localhost:5432/backupzit?sslmode=disable"
    fi
    if [ -z "$HOSTS" ]; then
        IP=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i <= NF; i++) if ($i == "src") print $(i + 1)}')
        HOSTS="${IP:+$IP,}$(hostname -f 2>/dev/null || hostname)"
    fi
    FIRST=${HOSTS%%,*}
    tmp=$(mktemp)
    grep -v '^BACKUPZIT_DB=\|^BACKUPZIT_TLS_HOSTS=\|^BACKUPZIT_PUBLIC_URL=' "$ENV" > "$tmp" || true
    {
        cat "$tmp"
        echo "BACKUPZIT_DB=$DB_URL"
        echo "BACKUPZIT_TLS_HOSTS=$HOSTS"
        echo "BACKUPZIT_PUBLIC_URL=https://$FIRST:8443"
    } > "$ENV"
    rm -f "$tmp"
    chown root:backupzit "$ENV"
    chmod 640 "$ENV"
fi

if [ $AUTO_UPDATES = 1 ]; then
    if [ $PM = apt ]; then
        printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades
    else
        sed -i 's/^upgrade_type.*/upgrade_type = security/; s/^apply_updates.*/apply_updates = yes/' /etc/dnf/automatic.conf 2>/dev/null || true
        systemctl enable --now dnf-automatic.timer >/dev/null 2>&1 || true
    fi
fi

if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
    firewall-cmd -q --permanent --add-port=8443/tcp && firewall-cmd -q --reload
elif command -v ufw >/dev/null 2>&1 && ufw status 2>/dev/null | grep -q 'Status: active'; then
    ufw allow 8443/tcp >/dev/null
fi

systemctl enable backupzit-server >/dev/null 2>&1 || true
systemctl restart backupzit-server
for i in $(seq 60); do
    curl -fsk -o /dev/null https://127.0.0.1:8443/login && break
    sleep 1
done
curl -fsk -o /dev/null https://127.0.0.1:8443/login || die "the console did not start; see: journalctl -u backupzit-server"

URL=$(sed -n 's/^BACKUPZIT_PUBLIC_URL=//p' /etc/backupzit/server.env)
echo
say "BackupZit $REL_VERSION is running"
echo "  Web console:  ${URL:-https://<this server>:8443}"
if [ $FRESH = 1 ] && [ -f /var/lib/backupzit/initial-admin-password.txt ]; then
    echo "  Sign in as:   admin, password: $(cat /var/lib/backupzit/initial-admin-password.txt)"
    echo "  Change the password after the first sign-in."
fi
echo "  The browser warns about the certificate once; see Settings -> Certificate for a trusted one."
