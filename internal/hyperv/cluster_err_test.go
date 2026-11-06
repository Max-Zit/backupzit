package hyperv

import (
	"errors"
	"testing"
)

func TestNotAppErr(t *testing.T) {
	err := notAppErr{"no VM could be backed up: dc: no application-consistent checkpoint (VSS)"}
	if !errors.Is(err, ErrNotAppConsistent) || err.Error() != "no VM could be backed up: dc: no application-consistent checkpoint (VSS)" {
		t.Errorf("%v", err)
	}
}
