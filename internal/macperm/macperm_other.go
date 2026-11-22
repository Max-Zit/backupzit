//go:build !darwin

package macperm

func fullDiskAccess() State { return NotApplicable }
