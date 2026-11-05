package repo

import (
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// Pack headers, index files and stored blobs come from the storage, which
// is not trusted: damaged or tampered data must give an error, not a panic
// or an allocation of all the memory.
func FuzzParsePackHeader(f *testing.F) {
	f.Add([]byte(`[{"type":1,"id":"` + Hash(nil).String() + `","offset":0,"length":12,"raw":0}]`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = parsePackHeader(b)
	})
}

func FuzzDecodeIndexFile(f *testing.F) {
	f.Add([]byte(`{"packs":[{"id":"` + Hash(nil).String() + `","blobs":[]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = decodeIndexFile(b)
	})
}

func FuzzParseID(f *testing.F) {
	f.Add(Hash([]byte("x")).String())
	f.Add("zz")
	f.Fuzz(func(t *testing.T, s string) {
		if id, err := ParseID(s); err == nil && !strings.EqualFold(id.String(), s) {
			t.Fatalf("ParseID(%q) = %s", s, id)
		}
	})
}

// FuzzDecodeBlob feeds stored data of an unencrypted repository (nothing
// authenticates it) with any declared size.
func FuzzDecodeBlob(f *testing.F) {
	enc, _ := zstd.NewWriter(nil)
	plain := []byte("hello backupzit")
	f.Add(enc.EncodeAll(plain, nil), uint32(len(plain)))
	f.Add([]byte{0x28, 0xb5, 0x2f, 0xfd, 0xff, 0xff, 0xff, 0xff}, uint32(1<<31))
	f.Add([]byte("raw"), uint32(0))
	dec, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(maxBlobRaw), zstd.WithDecoderMaxWindow(32<<20))
	if err != nil {
		f.Fatal(err)
	}
	r := &Repository{dec: dec}
	f.Fuzz(func(t *testing.T, stored []byte, raw uint32) {
		data, err := r.decodeBlob(Hash(plain), DataBlob, stored, raw)
		if err == nil && Hash(data) != Hash(plain) {
			t.Fatal("accepted data that does not match its ID")
		}
	})
}
