#!/bin/sh
# Builds the BackupZit appliance installer ISO from the official Debian
# netinst ISO: unattended installation of Debian, PostgreSQL and the
# BackupZit console (from the given server package).
#   ./build-iso.sh backupzit-server_X_amd64.deb [debian-netinst.iso]
# Needs: xorriso, isolinux, cpio, gzip, wget. The netinst ISO is downloaded
# from cdimage.debian.org and checked against Debian's SHA256SUMS.
set -eu
DEB=$(readlink -f "${1:?usage: build-iso.sh backupzit-server_X_amd64.deb [debian-netinst.iso]}")
VERSION=$(basename "$DEB" | sed -n 's/^backupzit-server_\([^_]*\)_.*/\1/p')
HERE=$(cd "$(dirname "$0")" && pwd)
OUT=${OUT:-$PWD/backupzit-appliance-$VERSION.iso}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

NETINST=${2:-}
if [ -z "$NETINST" ]; then
    NETINST=/var/cache/backupzit-appliance/debian-netinst.iso
    if [ ! -f "$NETINST" ]; then
        mkdir -p "$(dirname "$NETINST")"
        BASE=https://cdimage.debian.org/debian-cd/current/amd64/iso-cd
        wget -q -O "$WORK/SHA256SUMS" "$BASE/SHA256SUMS"
        NAME=$(awk '$2 ~ /netinst\.iso$/ && $2 !~ /edu|mac/ {print $2; exit}' "$WORK/SHA256SUMS")
        wget -q -O "$NETINST.part" "$BASE/$NAME"
        SUM=$(awk -v n="$NAME" '$2 == n {print $1}' "$WORK/SHA256SUMS")
        echo "$SUM  $NETINST.part" | sha256sum -c - >/dev/null
        mv "$NETINST.part" "$NETINST"
    fi
fi

xorriso -osirrox on -indev "$NETINST" -extract / "$WORK/iso" >/dev/null 2>&1
chmod -R u+w "$WORK/iso"

# BackupZit files on the ISO (copied into the new system by the preseed).
mkdir -p "$WORK/iso/backupzit"
cp "$DEB" "$HERE/install.sh" "$HERE/backupzit-firstboot" "$HERE/backupzit-firstboot.service" "$WORK/iso/backupzit/"
# IMAGE=1 builds the installer for VM images: marked for install.sh, and the
# installer powers off instead of restarting.
PRESEED="$HERE/preseed.cfg"
if [ "${IMAGE:-}" = 1 ]; then
    touch "$WORK/iso/backupzit/IMAGE"
    sed "s#^d-i debian-installer/exit/poweroff boolean false#d-i debian-installer/exit/poweroff boolean true#" "$HERE/preseed.cfg" > "$WORK/preseed.cfg"
    PRESEED="$WORK/preseed.cfg"
fi

# The preseed file inside the installer's initrd.
mkdir "$WORK/initrd"
(cd "$WORK/initrd" && gzip -dc "$WORK/iso/install.amd/initrd.gz" | cpio -id --quiet &&
    cp "$PRESEED" preseed.cfg &&
    find . | cpio -o -H newc --quiet | gzip -9 > "$WORK/iso/install.amd/initrd.gz")

# Boot menus (BIOS and UEFI): start the unattended installation after 5 seconds.
LABEL="BackupZit appliance $VERSION (erases the first disk)"
cat > "$WORK/iso/isolinux/isolinux.cfg" <<MENU
path
include menu.cfg
default vesamenu.c32
prompt 0
timeout 50
MENU
cat > "$WORK/iso/isolinux/menu.cfg" <<MENU
menu hshift 4
menu width 70
menu title BackupZit appliance installer
include stdmenu.cfg
default backupzit
label backupzit
	menu label ^Install $LABEL
	menu default
	kernel /install.amd/vmlinuz
	append auto=true priority=critical vga=788 initrd=/install.amd/initrd.gz --- quiet
MENU
cat > "$WORK/iso/boot/grub/grub.cfg" <<MENU
set timeout=5
set default=0
insmod all_video
menuentry "Install $LABEL" {
	linux /install.amd/vmlinuz auto=true priority=critical --- quiet
	initrd /install.amd/initrd.gz
}
MENU

(cd "$WORK/iso" && find . -type f ! -name md5sum.txt ! -path './isolinux/*' -print0 | xargs -0 md5sum > md5sum.txt)
xorriso -as mkisofs -r -V "BackupZit $VERSION" -o "$OUT" \
    -J -joliet-long -cache-inodes \
    -isohybrid-mbr /usr/lib/ISOLINUX/isohdpfx.bin \
    -b isolinux/isolinux.bin -c isolinux/boot.cat -boot-load-size 4 -boot-info-table -no-emul-boot \
    -eltorito-alt-boot -e boot/grub/efi.img -no-emul-boot -isohybrid-gpt-basdat \
    "$WORK/iso" >/dev/null 2>&1
echo "built $OUT"
