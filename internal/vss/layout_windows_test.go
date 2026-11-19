package vss

import (
	"runtime"
	"testing"
	"unsafe"
)

// VSS_OBJECT_PROP as the C headers lay it out on 64-bit Windows: the
// snapshot properties at offset 8, 136 bytes in all. Query() writes it.
func TestObjectPropLayout(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("Query() fallback is used on amd64 only")
	}
	var p vssObjectProp
	if s, o := unsafe.Sizeof(p), unsafe.Offsetof(p.snapshot); s != 136 || o != 8 {
		t.Fatalf("VSS_OBJECT_PROP: size %d, snapshot at %d; C has 136 and 8", s, o)
	}
}
