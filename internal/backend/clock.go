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
