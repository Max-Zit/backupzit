package winenc

import "testing"

// "BackupZitサーバー_Debian13" as written by a console program on Japanese
// Windows (code page 932, Shift-JIS) reads back correctly.
func TestShiftJIS(t *testing.T) {
	sjis := []byte("BackupZit\x83\x54\x81\x5b\x83\x6f\x81\x5b_Debian13")
	if got := decode(932, sjis); got != "BackupZitサーバー_Debian13" {
		t.Fatalf("got %q", got)
	}
	if got := Text([]byte("already UTF-8: サーバー")); got != "already UTF-8: サーバー" {
		t.Fatalf("UTF-8 changed: %q", got)
	}
}
