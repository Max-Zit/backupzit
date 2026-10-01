#!/bin/sh
set -e
if ! getent passwd backupzit >/dev/null; then
    useradd --system --home-dir /var/lib/backupzit --shell /usr/sbin/nologin backupzit 2>/dev/null \
      || useradd --system --home-dir /var/lib/backupzit --shell /sbin/nologin backupzit
fi
mkdir -p /var/lib/backupzit/dist
chown -R backupzit:backupzit /var/lib/backupzit
chmod 750 /var/lib/backupzit
chown root:backupzit /etc/backupzit/server.env
chmod 640 /etc/backupzit/server.env
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable backupzit-server.service >/dev/null 2>&1 || true
fi
echo "BackupZit server installed."
echo "1. Create a PostgreSQL database and user, then set BACKUPZIT_DB in /etc/backupzit/server.env"
echo "2. systemctl start backupzit-server"
echo "3. Open https://<this-host>:8443 (admin password: /var/lib/backupzit/initial-admin-password.txt)"
