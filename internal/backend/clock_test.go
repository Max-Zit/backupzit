package backend

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
)

// Storage that refuses a request because of the computer's clock gets the
// hint to correct the time; other errors stay as they are.
func TestClockHint(t *testing.T) {
	skew := minio.ErrorResponse{Code: "RequestTimeTooSkewed", Message: "The difference between the request time and the server's time is too large."}
	err := fmt.Errorf("s3: access bucket %q at %s: %w", "b", "host", withClockHint(skew))
	if !errors.Is(err, ErrClockSkew) || !strings.Contains(err.Error(), "correct the time") || !strings.Contains(err.Error(), "request time") {
		t.Fatalf("no clock hint: %v", err)
	}
	if e := withClockHint(withClockHint(skew)); strings.Count(e.Error(), "correct the time") != 1 {
		t.Fatalf("hint added twice: %v", e)
	}
	other := minio.ErrorResponse{Code: "AccessDenied", Message: "Access Denied."}
	if errors.Is(withClockHint(other), ErrClockSkew) {
		t.Fatal("hint on an unrelated error")
	}
	if withClockHint(nil) != nil {
		t.Fatal("nil changed")
	}
}

// A refused key gets a hint; for Backblaze B2 the one that explains its
// application keys (#68).
func TestKeyHint(t *testing.T) {
	b2 := minio.ErrorResponse{Code: "InvalidAccessKeyId", Message: "Malformed Access Key Id"}
	if e := withKeyHint(b2, "s3.us-west-004.backblazeb2.com"); !strings.Contains(e.Error(), "application key") || !strings.Contains(e.Error(), "Malformed") {
		t.Errorf("B2: %v", e)
	}
	if e := withKeyHint(minio.ErrorResponse{Code: "SignatureDoesNotMatch"}, "minio.lan"); !strings.Contains(e.Error(), "check the access key") {
		t.Errorf("other service: %v", e)
	}
	other := errors.New("connection refused")
	if withKeyHint(other, "s3.eu-central-003.backblazeb2.com") != other || withKeyHint(nil, "x") != nil {
		t.Error("unrelated errors changed")
	}
}

// White space around pasted keys is dropped before they are used (#68).
func TestOpenS3TrimsKeys(t *testing.T) {
	u, _ := url.Parse("s3://127.0.0.1:1/bucket")
	_, err := OpenS3(context.Background(), u, Options{S3AccessKey: " \n", S3SecretKey: "secret"})
	if err == nil || !strings.Contains(err.Error(), "access key and secret key are required") {
		t.Errorf("blank key accepted: %v", err)
	}
}
