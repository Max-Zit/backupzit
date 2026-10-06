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
# The BackupZit appliance (not other servers): the setup menu on its screen
# and hardened SSH settings.
if [ -f /var/lib/backupzit/.appliance-ready ] || [ -x /usr/local/sbin/backupzit-firstboot ]; then
    a=/usr/share/backupzit/appliance
    install -m 0644 $a/backupzit-menu.sh /etc/profile.d/backupzit-menu.sh
    install -m 0440 $a/sudoers-backupzit-setup /etc/sudoers.d/backupzit-setup
    install -m 0644 $a/sysctl-backupzit.conf /etc/sysctl.d/90-backupzit-console.conf
    sysctl -q -p /etc/sysctl.d/90-backupzit-console.conf 2>/dev/null || true
    # At every start: grow the root file system to an enlarged disk, and
    # write the login screen (it shows the initial password only until
    # admin's password is changed).
    install -m 0755 $a/backupzit-appliance-boot /usr/local/sbin/backupzit-appliance-boot
    install -m 0644 $a/backupzit-appliance-boot.service /etc/systemd/system/backupzit-appliance-boot.service
    systemctl daemon-reload 2>/dev/null || true
    systemctl enable backupzit-appliance-boot.service >/dev/null 2>&1 || true
    if [ -f /var/lib/backupzit/.appliance-ready ]; then
        /usr/local/sbin/backupzit-appliance-boot >/dev/null 2>&1 || true
    fi
    if [ -d /etc/ssh/sshd_config.d ]; then
        install -m 0644 $a/sshd-backupzit.conf /etc/ssh/sshd_config.d/50-backupzit.conf
        if sshd -t 2>/dev/null; then
            systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
        else
            rm -f /etc/ssh/sshd_config.d/50-backupzit.conf
        fi
    fi
fi
echo "BackupZit server installed."
echo "1. Create a PostgreSQL database and user, then set BACKUPZIT_DB in /etc/backupzit/server.env"
echo "2. systemctl start backupzit-server"
echo "3. Open https://<this-host>:8443 (admin password: /var/lib/backupzit/initial-admin-password.txt)"
