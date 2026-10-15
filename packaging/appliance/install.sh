#!/bin/sh
# Runs inside the installed system (preseed late_command).
set -e
cd /root/backupzit-install
# The firstboot script first: the package recognises the appliance by it.
install -m 0755 backupzit-firstboot /usr/local/sbin/backupzit-firstboot
dpkg -i backupzit-server_*.deb
install -m 0644 backupzit-firstboot.service /etc/systemd/system/backupzit-firstboot.service
systemctl enable backupzit-firstboot.service
# Automatic security updates (BackupZit shows their status under Settings → Updates).
printf 'APT::Periodic::Update-Package-Lists "1";\nAPT::Periodic::Unattended-Upgrade "1";\n' > /etc/apt/apt.conf.d/20auto-upgrades
# Where to get the source code of the GPL software, and the exact Debian
# packages of this installation.
mkdir -p /usr/share/doc/backupzit
install -m 0644 SOURCE-CODE.txt /usr/share/doc/backupzit/SOURCE-CODE.txt
dpkg-query -W -f '${Package} ${Version}\n' > /usr/share/doc/backupzit/debian-packages.txt
# The administrator chooses its own password at the first sign-in.
chage -d 0 bzadmin
usermod -aG sudo bzadmin

if [ -f IMAGE ]; then
    # A VM image (OVA, qcow2, VHDX) is copied many times and runs on any
    # hypervisor: DHCP on whatever network adapter it gets, and no host
    # identity baked in (SSH keys and machine ID are created at the first start).
    mkdir -p /etc/systemd/network
    printf '[Match]\nName=en* eth*\n\n[Network]\nDHCP=yes\n' > /etc/systemd/network/50-dhcp.network
    printf 'auto lo\niface lo inet loopback\n' > /etc/network/interfaces
    systemctl enable systemd-networkd systemd-networkd-wait-online
    # No DNS servers of the network the image was built in: they come from
    # the DHCP lease at the first start (backupzit-dns.path).
    echo '# Filled in by BackupZit from the DHCP lease.' > /etc/resolv.conf
    rm -f /etc/ssh/ssh_host_*
    truncate -s 0 /etc/machine-id
    rm -f /var/lib/dbus/machine-id
    apt-get clean
fi
