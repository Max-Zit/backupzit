package server

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/max-zit/backupzit/internal/api"
)

func vmBlocksForm(t *testing.T, old JobOptions, v *string) JobOptions {
	t.Helper()
	f := url.Values{}
	if v != nil {
		f.Set("vm_block_size_kib", *v)
	}
	r := httptest.NewRequest("POST", "/jobs", strings.NewReader(f.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := r.ParseForm(); err != nil {
		t.Fatal(err)
	}
	return optionsFromForm(r, old, false)
}

// Once a job used a block size other than 1 MiB, its repository may hold
// such images: agents without configurable block sizes must stay refused
// even after the job returns to 1 MiB.
func TestVMBlocksCustomIsSticky(t *testing.T) {
	s := func(v string) *string { return &v }
	o := vmBlocksForm(t, JobOptions{}, s("0"))
	if o.VMBlocksCustom {
		t.Fatal("default size marked custom")
	}
	o = vmBlocksForm(t, o, s("1024"))
	if o.VMBlocksCustom {
		t.Fatal("1 MiB marked custom")
	}
	o = vmBlocksForm(t, o, s("256"))
	if !o.VMBlocksCustom || o.VMBlockSizeKiB != 256 {
		t.Fatalf("256 KiB: %+v", o)
	}
	o = vmBlocksForm(t, o, s("0"))
	if !o.VMBlocksCustom || o.VMBlockSizeKiB != 0 {
		t.Fatalf("back to 1 MiB lost the flag: %+v", o)
	}
	o = vmBlocksForm(t, o, nil)
	if !o.VMBlocksCustom {
		t.Fatal("form without the field lost the flag")
	}
	if bad := vmBlocksForm(t, JobOptions{}, s("abc")); bad.Validate() == nil {
		t.Fatal("malformed size accepted")
	}

	for _, c := range []struct {
		o    JobOptions
		kind string
		need bool
	}{
		{JobOptions{}, api.KindVMBackup, false},
		{JobOptions{VMBlockSizeKiB: 1024}, api.KindVMBackup, false},
		{JobOptions{VMBlockSizeKiB: 512}, api.KindVMBackup, true},
		{JobOptions{VMBlocksCustom: true}, api.KindVMBackup, true},
		{JobOptions{VMBlockSizeKiB: 256, VMBlocksCustom: true}, api.KindBackup, false},
	} {
		ar := &api.Run{}
		c.o.apply(ar, c.kind)
		if ar.Repository.NeedsVMBlocks != c.need {
			t.Errorf("%+v %s: NeedsVMBlocks=%v", c.o, c.kind, ar.Repository.NeedsVMBlocks)
		}
	}
}
