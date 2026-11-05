//go:build !linux && !darwin

package doctor

import (
	"errors"
	"runtime"
)

// diskFree is not implemented here; the check reports it as unmeasured.
func diskFree(string) (uint64, error) {
	return 0, errors.New("not measured on " + runtime.GOOS)
}
