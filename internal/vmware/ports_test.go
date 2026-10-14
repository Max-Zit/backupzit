package vmware

import (
	"context"
	"crypto/tls"
	"testing"

	"github.com/vmware/govmomi/object"
	"github.com/vmware/govmomi/simulator"
	"github.com/vmware/govmomi/vim25/mo"
	"github.com/vmware/govmomi/vim25/types"
)

// TestMovePortFiles points a serial port file left on the old datastore
// into the VM's folder (after Storage vMotion) and leaves other ports alone.
func TestMovePortFiles(t *testing.T) {
	ctx := context.Background()
	model := simulator.VPX()
	defer model.Remove()
	if err := model.Create(); err != nil {
		t.Fatal(err)
	}
	model.Service.TLS = new(tls.Config)
	s := model.Service.NewServer()
	defer s.Close()
	pw, _ := s.URL.User.Password()
	c, err := Connect(ctx, Conn{Host: s.URL.Host, User: s.URL.User.Username(), Password: pw})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Logout()
	vms, err := c.VMs(ctx)
	if err != nil || len(vms) == 0 {
		t.Fatal(vms, err)
	}
	vm := object.NewVirtualMachine(c.vim, vmRef(vms[0].MoID))
	port := func(file string) *types.VirtualSerialPort {
		return &types.VirtualSerialPort{VirtualDevice: types.VirtualDevice{Key: -1, Backing: &types.VirtualSerialPortFileBackingInfo{
			VirtualDeviceFileBackingInfo: types.VirtualDeviceFileBackingInfo{FileName: file}}}}
	}
	spec := types.VirtualMachineConfigSpec{DeviceChange: []types.BaseVirtualDeviceConfigSpec{
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: port("[backupzit-nfs] old/serial.log")},
		&types.VirtualDeviceConfigSpec{Operation: types.VirtualDeviceConfigSpecOperationAdd, Device: port("[other] keep/com2.log")},
	}}
	tk, err := vm.Reconfigure(ctx, spec)
	if err := c.wait(ctx, tk, err); err != nil {
		t.Fatal(err)
	}
	if err := c.movePortFiles(ctx, vm, "backupzit-nfs"); err != nil {
		t.Fatal(err)
	}
	var m mo.VirtualMachine
	if err := vm.Properties(ctx, vm.Reference(), []string{"config.hardware.device", "config.files.vmPathName"}, &m); err != nil {
		t.Fatal(err)
	}
	vmx, _ := parseDSPath(m.Config.Files.VmPathName)
	var files []string
	for _, d := range m.Config.Hardware.Device {
		if p, ok := d.(*types.VirtualSerialPort); ok {
			if b, ok := p.Backing.(*types.VirtualSerialPortFileBackingInfo); ok {
				files = append(files, b.FileName)
			}
		}
	}
	want := map[string]bool{vmx.join("serial.log").String(): true, "[other] keep/com2.log": true}
	if len(files) != 2 || !want[files[0]] || !want[files[1]] {
		t.Errorf("port files %v, want %v", files, want)
	}
}
