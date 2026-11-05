package platform

import (
	"errors"
	"os"
	"path/filepath"
)

// WritePrivate writes data to path atomically with mode 0600: a fresh
// temp file (never a fixed name another process could have planted or
// left with wider bits) in the same folder, chmod 0600, then a rename over
// path. A reader sees the old file or the new one, never half of one, and
// a failure leaves no temp file behind. The folder must exist.
func WritePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, werr := f.Write(data)
	if err := errors.Join(werr, f.Chmod(0o600), f.Close()); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
