#!/bin/sh
set -e
if ! getent passwd backupzit-repo >/dev/null; then
    useradd --system --home-dir /srv/backupzit --shell /usr/sbin/nologin backupzit-repo 2>/dev/null \
      || useradd --system --home-dir /srv/backupzit --shell /sbin/nologin backupzit-repo
fi
mkdir -p /srv/backupzit /etc/backupzit-repo
chown backupzit-repo:backupzit-repo /srv/backupzit
chmod 700 /srv/backupzit
# The configuration directory is root's; the service only reads its keys
# and certificate, through its group. In a directory of the service's, a
# compromised service could put links where root changes files here (up to
# 0.55 it was the service's, so links found below are left alone).
chown -h root:backupzit-repo /etc/backupzit-repo
chmod 750 /etc/backupzit-repo
for f in repo.env keys cert.pem key.pem; do
    p=/etc/backupzit-repo/$f
    if [ -f "$p" ] && [ ! -L "$p" ]; then
        chown -h root:backupzit-repo "$p"
        chmod 640 "$p"
    fi
done
# Create the certificate (root's, readable for the service's group).
/usr/bin/backupzit-repo fingerprint --config-dir /etc/backupzit-repo >/dev/null || true
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable backupzit-repo.service >/dev/null 2>&1 || true
fi
echo "BackupZit hardened repository installed."
echo "1. Review /etc/backupzit-repo/repo.env (immutability period, storage directory)"
echo "2. systemctl start backupzit-repo"
echo "3. backupzit-repo add-key <name>     -> access key for the BackupZit console"
echo "   backupzit-repo fingerprint        -> certificate fingerprint for the console"
echo "4. Add a 'Hardened repository' storage target in the console, then disable SSH"
echo "   and other remote access to this server."
