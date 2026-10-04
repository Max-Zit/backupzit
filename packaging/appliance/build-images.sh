#!/bin/sh
# Builds the BackupZit appliance as ready-made VM images on a Proxmox VE host:
# installs the appliance ISO (IMAGE=1) into a temporary VM and converts its disk to
#   backupzit-appliance-X.ova    VMware ESXi/Workstation, VirtualBox
#   backupzit-appliance-X.qcow2  Proxmox VE, KVM
#   backupzit-appliance-X.vhdx   Hyper-V (generation 1)
#   ./build-images.sh backupzit-server_X_amd64.deb
# Environment: VMID (default 9400), STORAGE (local-lvm), BRIDGE (vmbr0), OUTDIR (.).
set -eu
DEB=$(readlink -f "${1:?usage: build-images.sh backupzit-server_X_amd64.deb}")
VERSION=$(basename "$DEB" | sed -n 's/^backupzit-server_\([^_]*\)_.*/\1/p')
HERE=$(cd "$(dirname "$0")" && pwd)
VMID=${VMID:-9400}
STORAGE=${STORAGE:-local-lvm}
BRIDGE=${BRIDGE:-vmbr0}
OUTDIR=$(readlink -f "${OUTDIR:-.}")
NAME=backupzit-appliance-$VERSION
ISO=/var/lib/vz/template/iso/backupzit-image-installer-$VERSION.iso
DISK_GB=16

cleanup() {
    qm stop "$VMID" >/dev/null 2>&1 || true
    qm destroy "$VMID" --purge >/dev/null 2>&1 || true
    rm -f "$ISO"
}
trap cleanup EXIT

IMAGE=1 OUT="$ISO" "$HERE/build-iso.sh" "$DEB" >/dev/null
qm create "$VMID" --name "bz-image-build" --memory 2048 --cores 2 --net0 "virtio,bridge=$BRIDGE" \
    --scsihw virtio-scsi-pci --scsi0 "$STORAGE:$DISK_GB" --ide2 "local:iso/$(basename "$ISO"),media=cdrom" \
    --boot 'order=scsi0;ide2' --ostype l26 >/dev/null
qm start "$VMID"
echo "installing into VM $VMID (the installer powers off when done)..."
i=0
while [ "$(qm status "$VMID" | awk '{print $2}')" != stopped ]; do
    i=$((i + 1))
    [ $i -lt 720 ] || { echo "installation did not finish within an hour" >&2; exit 1; }
    sleep 5
done
VOL=$(qm config "$VMID" | sed -n 's/^scsi0: \([^,]*\).*/\1/p')
SRC=$(pvesm path "$VOL")
cd "$OUTDIR"
echo "converting $SRC..."
qemu-img convert -c -O qcow2 "$SRC" "$NAME.qcow2"
qemu-img convert -O vhdx -o subformat=dynamic "$SRC" "$NAME.vhdx"
qemu-img convert -O vmdk -o subformat=streamOptimized,adapter_type=lsilogic "$SRC" "$NAME-disk1.vmdk"

CAP=$((DISK_GB * 1024 * 1024 * 1024))
SIZE=$(stat -c %s "$NAME-disk1.vmdk")
cat > "$NAME.ovf" <<OVF
<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1" xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
  xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData"
  xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData"
  xmlns:vmw="http://www.vmware.com/schema/ovf">
  <References>
    <File ovf:href="$NAME-disk1.vmdk" ovf:id="file1" ovf:size="$SIZE"/>
  </References>
  <DiskSection>
    <Info>Virtual disks</Info>
    <Disk ovf:capacity="$CAP" ovf:capacityAllocationUnits="byte" ovf:diskId="vmdisk1" ovf:fileRef="file1"
      ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
  </DiskSection>
  <NetworkSection>
    <Info>Networks</Info>
    <Network ovf:name="VM Network"><Description>The network on which the console is reached</Description></Network>
  </NetworkSection>
  <VirtualSystem ovf:id="$NAME">
    <Info>BackupZit backup appliance $VERSION</Info>
    <Name>BackupZit</Name>
    <AnnotationSection>
      <Info>Description</Info>
      <Annotation>BackupZit backup appliance $VERSION. The console's address and initial password are shown on the VM's screen after the first start.</Annotation>
    </AnnotationSection>
    <OperatingSystemSection ovf:id="96" vmw:osType="debian12_64Guest">
      <Info>Debian Linux (64-bit)</Info>
    </OperatingSystemSection>
    <VirtualHardwareSection>
      <Info>Virtual hardware</Info>
      <System>
        <vssd:ElementName>Virtual Hardware Family</vssd:ElementName>
        <vssd:InstanceID>0</vssd:InstanceID>
        <vssd:VirtualSystemIdentifier>BackupZit</vssd:VirtualSystemIdentifier>
        <vssd:VirtualSystemType>vmx-14</vssd:VirtualSystemType>
      </System>
      <Item>
        <rasd:AllocationUnits>hertz * 10^6</rasd:AllocationUnits>
        <rasd:Description>Number of virtual CPUs</rasd:Description>
        <rasd:ElementName>2 virtual CPUs</rasd:ElementName>
        <rasd:InstanceID>1</rasd:InstanceID>
        <rasd:ResourceType>3</rasd:ResourceType>
        <rasd:VirtualQuantity>2</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:AllocationUnits>byte * 2^20</rasd:AllocationUnits>
        <rasd:Description>Memory size</rasd:Description>
        <rasd:ElementName>2048 MB of memory</rasd:ElementName>
        <rasd:InstanceID>2</rasd:InstanceID>
        <rasd:ResourceType>4</rasd:ResourceType>
        <rasd:VirtualQuantity>2048</rasd:VirtualQuantity>
      </Item>
      <Item>
        <rasd:Address>0</rasd:Address>
        <rasd:Description>SCSI Controller</rasd:Description>
        <rasd:ElementName>SCSI controller 0</rasd:ElementName>
        <rasd:InstanceID>3</rasd:InstanceID>
        <rasd:ResourceSubType>lsilogic</rasd:ResourceSubType>
        <rasd:ResourceType>6</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>0</rasd:AddressOnParent>
        <rasd:ElementName>Hard disk 1</rasd:ElementName>
        <rasd:HostResource>ovf:/disk/vmdisk1</rasd:HostResource>
        <rasd:InstanceID>4</rasd:InstanceID>
        <rasd:Parent>3</rasd:Parent>
        <rasd:ResourceType>17</rasd:ResourceType>
      </Item>
      <Item>
        <rasd:AddressOnParent>7</rasd:AddressOnParent>
        <rasd:AutomaticAllocation>true</rasd:AutomaticAllocation>
        <rasd:Connection>VM Network</rasd:Connection>
        <rasd:ElementName>Network adapter 1</rasd:ElementName>
        <rasd:InstanceID>5</rasd:InstanceID>
        <rasd:ResourceSubType>VmxNet3</rasd:ResourceSubType>
        <rasd:ResourceType>10</rasd:ResourceType>
      </Item>
    </VirtualHardwareSection>
  </VirtualSystem>
</Envelope>
OVF
{
    echo "SHA256($NAME.ovf)= $(sha256sum "$NAME.ovf" | cut -d' ' -f1)"
    echo "SHA256($NAME-disk1.vmdk)= $(sha256sum "$NAME-disk1.vmdk" | cut -d' ' -f1)"
} > "$NAME.mf"
# The descriptor must come first in an OVA.
tar --format=ustar -cf "$NAME.ova" "$NAME.ovf" "$NAME.mf" "$NAME-disk1.vmdk"
rm -f "$NAME.ovf" "$NAME.mf" "$NAME-disk1.vmdk"
sha256sum "$NAME.ova" "$NAME.qcow2" "$NAME.vhdx" > "$NAME.SHA256SUMS"
ls -l "$NAME".*
