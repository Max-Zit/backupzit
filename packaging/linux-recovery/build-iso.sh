#!/bin/sh
# Builds the BackupZit Linux recovery ISO: a small Debian 12 live system
# (BIOS and UEFI, CD or USB) that starts the agent in recovery mode, with the
# tools a Linux system restore needs (gdisk, LVM, ext4/XFS/FAT tools).
#
#   sudo ./build-iso.sh <version> <path/to/backupzit-agent (linux/amd64)>
#
# Runs on Debian 12 (or a Proxmox VE node) with live-build installed:
#   apt install live-build
# Output: backupzit-recovery-linux-<version>.iso in the current directory.
set -eu
VERSION=${1:?usage: build-iso.sh <version> <agent binary>}
AGENT=${2:?usage: build-iso.sh <version> <agent binary>}
OUT=$(pwd)
WORK=${WORK:-/var/tmp/backupzit-live}
SRC=$(cd "$(dirname "$0")" && pwd)

rm -rf "$WORK" && mkdir -p "$WORK" && cd "$WORK"
lb config \
    --distribution bookworm \
    --archive-areas "main" \
    --debian-installer none \
    --binary-images iso-hybrid \
    --apt-recommends false \
    --memtest none \
    --iso-volume "BACKUPZIT_RECOVERY" \
    --iso-application "BackupZit recovery" \
    --iso-publisher "MaxZit" \
    --bootappend-live "boot=live components quiet hostname=backupzit-recovery username=root"

# Boot the live system automatically after 5 seconds (BIOS and UEFI menus).
mkdir -p config/bootloaders
cp -r /usr/share/live/build/bootloaders/isolinux /usr/share/live/build/bootloaders/grub-pc config/bootloaders/
sed -i "s/^timeout .*/timeout 50/" config/bootloaders/isolinux/isolinux.cfg
printf "
set timeout=5
" >> config/bootloaders/grub-pc/config.cfg

cat > config/package-lists/backupzit.list.chroot <<'PKGS'
live-boot
live-config
live-config-systemd
systemd-sysv
linux-image-amd64
firmware-linux-free
util-linux
fdisk
gdisk
parted
dosfstools
e2fsprogs
xfsprogs
lvm2
kmod
udev
iproute2
iputils-ping
ca-certificates
less
nano
openssh-client
PKGS

mkdir -p config/includes.chroot/usr/local/bin config/includes.chroot/etc/systemd/network \
         config/includes.chroot/etc/systemd/system/getty@tty1.service.d config/includes.chroot/etc/profile.d config/hooks/live
install -m 0755 "$AGENT" config/includes.chroot/usr/local/bin/backupzit-agent
install -m 0755 "$SRC/bz-recovery-start" config/includes.chroot/usr/local/bin/bz-recovery-start
cat > config/includes.chroot/etc/systemd/network/80-dhcp.network <<'NET'
[Match]
Name=en* eth*

[Network]
DHCP=yes
NET
cat > config/includes.chroot/etc/systemd/system/getty@tty1.service.d/autologin.conf <<'GETTY'
[Service]
ExecStart=
ExecStart=-/sbin/agetty --autologin root --noclear %I $TERM
GETTY
cat > config/includes.chroot/etc/profile.d/zz-backupzit.sh <<'PROFILE'
# Start the recovery agent on the first console.
if [ "$(tty)" = /dev/tty1 ] && [ -z "${BZ_STARTED:-}" ]; then
    export BZ_STARTED=1
    /usr/local/bin/bz-recovery-start
fi
PROFILE
cat > config/hooks/live/0100-backupzit.hook.chroot <<'HOOK'
#!/bin/sh
set -e
systemctl enable systemd-networkd
echo "BackupZit recovery @VERSION@" > /etc/backupzit-recovery
HOOK
sed -i "s/@VERSION@/$VERSION/" config/hooks/live/0100-backupzit.hook.chroot

lb build
mv live-image-amd64.hybrid.iso "$OUT/backupzit-recovery-linux-$VERSION.iso"
echo "built $OUT/backupzit-recovery-linux-$VERSION.iso"
