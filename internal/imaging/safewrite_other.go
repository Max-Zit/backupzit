//go:build !windows

package imaging

import (
	"errors"
	"os"
)

// createStream: alternate data streams exist only on Windows (Create never
// asks for one elsewhere).
func (t *Target) createStream(string) (*os.File, error) {
	return nil, errors.New("alternate data streams are written only on Windows")
}
