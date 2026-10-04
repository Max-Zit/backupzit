#!/bin/sh
set -e
if ! getent passwd backupzit >/dev/null; then
    useradd --system --home-dir /var/lib/backupzit --shell /usr/sbin/nologin backupzit 2>/dev/null \
      || useradd --system --home-dir /var/lib/backupzit --shell /sbin/nologin backupzit
fi
mkdir -p /var/lib/backupzit/dist /var/lib/backupzit/update
chown -R backupzit:backupzit /var/lib/backupzit
chmod 750 /var/lib/backupzit
# Offer the bundled agent installers for download (keeps older ones).
if [ -d /usr/share/backupzit/dist ]; then
    cp -f /usr/share/backupzit/dist/* /var/lib/backupzit/dist/ 2>/dev/null || true
    chown -R backupzit:backupzit /var/lib/backupzit/dist
fi
chown root:backupzit /etc/backupzit/server.env
chmod 640 /etc/backupzit/server.env
if command -v systemctl >/dev/null 2>&1; then
    systemctl daemon-reload || true
    systemctl enable backupzit-server.service >/dev/null 2>&1 || true
    # Updates requested in the web console are installed by a root helper.
    systemctl enable --now backupzit-update.path >/dev/null 2>&1 || true
fi
echo "BackupZit server installed."
echo "1. Create a PostgreSQL database and user, then set BACKUPZIT_DB in /etc/backupzit/server.env"
echo "2. systemctl start backupzit-server"
echo "3. Open https://<this-host>:8443 (admin password: /var/lib/backupzit/initial-admin-password.txt)"
