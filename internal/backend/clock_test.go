package backend

import (
	"errors"
	"fmt"
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
