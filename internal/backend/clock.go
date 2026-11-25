package backend

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/minio/minio-go/v7"
)

// clockHint explains errors of cloud storage that refuses requests from a
// computer whose clock is wrong (S3 and Azure sign requests with the time
// and reject them when it is more than about 15 minutes off).
const clockHint = "the clock of this computer differs from the storage's by more than 15 minutes: correct the time (turn on time synchronization)"

// ErrClockSkew is wrapped by errors of storage that refused a request
// because of the computer's clock.
var ErrClockSkew = errors.New(clockHint)

// withClockHint adds the clock hint to err when the storage refused the
// request because of the time.
// withKeyHint explains what an S3 service's "unusable key" answer usually
// means (#68: Backblaze B2 answers "Malformed Access Key Id" for its master
// key, which its S3 API does not accept).
func withKeyHint(err error, host string) error {
	if err == nil {
		return nil
	}
	code := minio.ToErrorResponse(err).Code
	if code != "InvalidAccessKeyId" && code != "SignatureDoesNotMatch" && !strings.Contains(err.Error(), "Malformed Access Key Id") {
		return err
	}
	hint := "check the access key and the secret key"
	if strings.HasSuffix(strings.ToLower(host), ".backblazeb2.com") {
		hint = "Backblaze B2 needs an application key, its keyID as the access key and its applicationKey as the secret key, not the master key; the region is the one in the endpoint s3.<region>.backblazeb2.com"
	}
	return fmt.Errorf("%w (%s)", err, hint)
}

func withClockHint(err error) error {
	if err == nil || errors.Is(err, ErrClockSkew) {
		return err
	}
	skew := minio.ToErrorResponse(err).Code == "RequestTimeTooSkewed"
	var re *azcore.ResponseError
	if errors.As(err, &re) && re.ErrorCode == "AuthenticationFailed" && strings.Contains(re.Error(), "Request date header too old") {
		skew = true
	}
	if !skew {
		return err
	}
	return fmt.Errorf("%w (%w)", err, ErrClockSkew)
}
