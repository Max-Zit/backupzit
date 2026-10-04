#!/bin/sh
# Runs inside the installed system (preseed late_command).
set -e
cd /root/backupzit-install
dpkg -i backupzit-server_*.deb
install -m 0755 backupzit-firstboot /usr/local/sbin/backupzit-firstboot
install -m 0644 backupzit-firstboot.service /etc/systemd/system/backupzit-firstboot.service
systemctl enable backupzit-firstboot.service
# Automatic security updates (BackupZit shows their status under Settings → Updates).
printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades
# The administrator chooses its own password at the first sign-in.
chage -d 0 bzadmin
usermod -aG sudo bzadmin
