package backend

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"testing"
	"time"

	"github.com/cloudsoda/go-smb2"
)

// A real SMB server is required; credentials must be supplied separately from
// the URL. Each test gets a unique directory below the configured test root.
func testSMB(t *testing.T) (*SMB, *url.URL, Options) {
	t.Helper()
	location := os.Getenv("BACKUPZIT_TEST_SMB_URL")
	if location == "" {
		t.Skip("set BACKUPZIT_TEST_SMB_URL to a disposable SMB test root")
	}
	u, err := url.Parse(location)
	if err != nil {
		t.Fatal("invalid SMB test URL")
	}
	u.Path = path.Join(u.Path, fmt.Sprintf("test-%d", time.Now().UnixNano()))
	opts := Options{SMBPassword: os.Getenv("BACKUPZIT_TEST_SMB_PASSWORD"), SMBDomain: os.Getenv("BACKUPZIT_TEST_SMB_DOMAIN")}
	be, err := OpenSMB(context.Background(), u, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		names, err := be.List(context.Background(), "")
		if err != nil {
			t.Error(err)
		} else {
			for _, name := range names {
				if err := be.Remove(context.Background(), name); err != nil {
					t.Error(err)
				}
			}
		}
		if err := be.RemoveDirs(context.Background()); err != nil {
			t.Error(err)
		}
		_ = be.Close()
	})
	return be, u, opts
}

func TestSMBSaveReplacesExistingObject(t *testing.T) {
	be, u, opts := testSMB(t)
	reader, err := OpenSMB(context.Background(), u, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	for _, value := range [][]byte{[]byte("original long contents"), []byte("new"), {}, bytes.Repeat([]byte("x"), 70000)} {
		if err := be.Save(ctx, "locks/test", value); err != nil {
			t.Fatal(err)
		}
		got, err := reader.Load(ctx, "locks/test")
		if err != nil || !bytes.Equal(got, value) {
			t.Fatalf("successful Save did not replace contents: got %d bytes, want %d, error %v", len(got), len(value), err)
		}
		if size, err := reader.Size(ctx, "locks/test"); err != nil || size != int64(len(value)) {
			t.Fatalf("size=%d want=%d error=%v", size, len(value), err)
		}
	}
}

func TestSMBSaveRenameFailurePreservesTarget(t *testing.T) {
	be, _, _ := testSMB(t)
	ctx := context.Background()
	if err := be.Save(ctx, "locks/test", []byte("original")); err != nil {
		t.Fatal(err)
	}
	// The library opens without FILE_SHARE_DELETE. Windows SMB refuses
	// replacement while this handle is open: Save must report that failure.
	f, err := be.share.Open(be.p("locks/test"))
	if err != nil {
		t.Fatal(err)
	}
	err = be.Save(ctx, "locks/test", []byte("replacement"))
	_ = f.Close()
	if err == nil {
		t.Fatal("Save hid a failed rename behind an existing target")
	}
	got, err := be.Load(ctx, "locks/test")
	if err != nil || string(got) != "original" {
		t.Fatalf("failed replacement changed target: %q %v", got, err)
	}
	entries, err := be.share.ReadDir(be.p("locks"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "test" {
		t.Fatalf("temporary files left after failed replacement: %v %v", entries, err)
	}
	if err := be.Save(ctx, "locks/test", []byte("replacement")); err != nil {
		t.Fatalf("retry after releasing reader: %v", err)
	}
}

func TestSMBSaveConcurrentReaders(t *testing.T) {
	be, u, opts := testSMB(t)
	reader, err := OpenSMB(context.Background(), u, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	ctx := context.Background()
	values := [][]byte{bytes.Repeat([]byte("a"), 128000), bytes.Repeat([]byte("b"), 192000)}
	if err := be.Save(ctx, "locks/test", values[0]); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	result := make(chan error, 1)
	ready := make(chan struct{})
	go func() {
		close(ready)
		reads := 0
		for {
			select {
			case <-stop:
				if reads == 0 {
					result <- errors.New("no complete read during replacement")
				} else {
					result <- nil
				}
				return
			default:
			}
			data, err := reader.Load(ctx, "locks/test")
			if err != nil {
				if smbSharingViolation(err) {
					continue // a reported sharing error is not a missing/partial file
				}
				result <- fmt.Errorf("read during replacement: %w", err)
				return
			}
			if !bytes.Equal(data, values[0]) && !bytes.Equal(data, values[1]) {
				result <- fmt.Errorf("reader observed partial or mixed contents (%d bytes)", len(data))
				return
			}
			reads++
		}
	}()
	<-ready
	writes, busy := 0, 0
	for i := 0; i < 32; i++ {
		if err := be.Save(ctx, "locks/test", values[i%2]); err != nil {
			// An open reader can temporarily deny delete sharing. Reporting
			// failure is correct; removing/truncating the live target is not.
			if !smbSharingViolation(err) && !errors.Is(err, os.ErrPermission) {
				close(stop)
				<-result
				t.Fatalf("unexpected save error: %v", err)
			}
			busy++
		} else {
			writes++
		}
	}
	close(stop)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if writes == 0 {
		t.Fatal("no replacement succeeded during concurrent reads")
	}
	t.Logf("concurrent reads: %d replacements, %d reported busy failures", writes, busy)
	if err := be.Save(ctx, "locks/test", []byte("final")); err != nil {
		t.Fatal(err)
	}
	got, err := reader.Load(ctx, "locks/test")
	if err != nil || string(got) != "final" {
		t.Fatalf("final replacement: %q %v", got, err)
	}
}

func smbSharingViolation(err error) bool {
	var response *smb2.ResponseError
	return errors.As(err, &response) && response.Code == 0xc0000043 // STATUS_SHARING_VIOLATION
}
