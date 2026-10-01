#!/bin/sh
set -e
if ! getent passwd backupzit-repo >/dev/null; then
    useradd --system --home-dir /srv/backupzit --shell /usr/sbin/nologin backupzit-repo 2>/dev/null \
      || useradd --system --home-dir /srv/backupzit --shell /sbin/nologin backupzit-repo
fi
mkdir -p /srv/backupzit /etc/backupzit-repo
chown backupzit-repo:backupzit-repo /srv/backupzit /etc/backupzit-repo
chmod 700 /srv/backupzit /etc/backupzit-repo
chown root:backupzit-repo /etc/backupzit-repo/repo.env
chmod 640 /etc/backupzit-repo/repo.env
# Create the certificate as the service user.
if command -v runuser >/dev/null 2>&1; then
    runuser -u backupzit-repo -- /usr/bin/backupzit-repo fingerprint --config-dir /etc/backupzit-repo >/dev/null || true
fi
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable backupzit-repo.service >/dev/null 2>&1 || true
fi
echo "backupzit hardened repository installed."
echo "1. Review /etc/backupzit-repo/repo.env (immutability period, storage directory)"
echo "2. systemctl start backupzit-repo"
echo "3. backupzit-repo add-key <name>     -> access key for the backupzit console"
echo "   backupzit-repo fingerprint        -> certificate fingerprint for the console"
echo "4. Add a 'Hardened repository' storage target in the console, then disable SSH"
echo "   and other remote access to this server."
