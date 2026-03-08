//go:build linux

package connkill

import (
	"errors"
	"testing"
)

func TestIsPermissionError_True(t *testing.T) {
	err := errors.New("operation not permitted: permission denied")
	if !isPermissionError(err) {
		t.Error("expected isPermissionError=true for 'permission denied' error")
	}
}

func TestIsPermissionError_False(t *testing.T) {
	err := errors.New("some other error")
	if isPermissionError(err) {
		t.Error("expected isPermissionError=false for non-permission error")
	}
}

func TestIsPermissionError_Nil(t *testing.T) {
	if isPermissionError(nil) {
		t.Error("expected isPermissionError=false for nil error")
	}
}

func TestIsPermissionError_CaseInsensitive(t *testing.T) {
	// The function checks for exact "permission denied" (lowercase)
	err := errors.New("PERMISSION DENIED")
	if isPermissionError(err) {
		t.Error("expected isPermissionError=false for uppercase (function is case-sensitive)")
	}
}
