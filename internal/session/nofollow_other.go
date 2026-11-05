//go:build !unix

package session

// oNoFollow is not available here; symlink planting needs a sandbox, which
// is Linux only.
const oNoFollow = 0
