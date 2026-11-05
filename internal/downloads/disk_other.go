//go:build !unix

package downloads

import "errors"

const oNoFollow = 0

// freeSpace is unknown here; the disk check is skipped.
func freeSpace(string) (uint64, error) { return 0, errors.New("free space unknown on this platform") }
