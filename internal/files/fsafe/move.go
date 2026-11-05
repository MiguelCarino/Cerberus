package fsafe

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// TempPrefix starts the names of files still being written (internal/files
// streams an upload into "<TempPrefix><hex>.part" before linking it into
// place). Deliveries and listings skip them.
const TempPrefix = ".cerberus-upload-"

// PrivateList names, one per line, the files of a held folder (a staging
// folder or a session's attach folder, both under DATA) that must stay
// private (0600) once delivered into a workspace: their original names
// looked secret-ish, which the cleaned name may no longer show (".env"
// waits as "env"). Files in a held folder are all 0600 while they wait
// (HeldMode), so their mode cannot carry that. The TempPrefix keeps the
// list out of listings and deliveries.
const PrivateList = TempPrefix + "private"

// HeldMode is the mode of a file waiting in a held folder: 0600, plus the
// owner's exec bit when allowExec and exec.
func HeldMode(allowExec, exec bool) fs.FileMode {
	if allowExec && exec {
		return 0o700
	}
	return 0o600
}

// InitHeld makes dir a held folder: it creates dir's PrivateList when
// there is none, listing the files already there that are private by mode
// (written before the folder was held), so none of them loses that.
func InitHeld(dir string) error {
	f, err := os.OpenFile(filepath.Join(dir, PrivateList), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var b strings.Builder
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), TempPrefix) {
			continue
		}
		if st, err := e.Info(); err == nil && st.Mode().Perm()&0o077 == 0 {
			b.WriteString(e.Name() + "\n")
		}
	}
	_, werr := io.WriteString(f, b.String())
	return errors.Join(werr, f.Close())
}

// MarkPrivate records name in dir's PrivateList.
func MarkPrivate(dir, name string) error {
	f, err := os.OpenFile(filepath.Join(dir, PrivateList), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	_, werr := io.WriteString(f, name+"\n")
	return errors.Join(werr, f.Close())
}

// privateNames reads dir's PrivateList; held is false when it has none.
func privateNames(dir string) (names map[string]bool, held bool) {
	raw, err := os.ReadFile(filepath.Join(dir, PrivateList))
	if err != nil {
		return nil, false
	}
	names = map[string]bool{}
	for _, l := range strings.Split(string(raw), "\n") {
		if l != "" {
			names[l] = true
		}
	}
	return names, true
}

// Moved is one file MoveInto or CopyInto delivered.
type Moved struct {
	Name string // the name it got in the destination (collisions renamed)
	Size int64
}

// FileMode is the mode a dropped file gets: 0644, or 0600 when either name
// looks secret-ish. With allowExec and exec set, the owner (and, for 0644,
// everyone who may read) also gets the exec bit; without allowExec the exec
// bit is never set.
func FileMode(name, original string, allowExec, exec bool) fs.FileMode {
	secret := Secretish(name) || (original != "" && Secretish(original))
	m := fs.FileMode(0o644)
	if secret {
		m = 0o600
	}
	if allowExec && exec {
		if secret {
			m |= 0o100
		} else {
			m |= 0o111
		}
	}
	return m
}

// MoveInto moves every regular file at the top of src into the folder dst
// (created 0755 if missing), renaming collisions to "name (2).ext", fixing
// each file's mode with FileMode, and removes src afterwards. dst must not
// be a symlink. Files that already arrived stay where they are when a later
// one fails.
func MoveInto(src, dst string, allowExec bool) ([]Moved, error) {
	return deliver(src, dst, allowExec, true, false)
}

// MoveIntoHeld is MoveInto into a held folder dst (created 0700): every
// file gets HeldMode, and the private ones are recorded in dst's
// PrivateList.
func MoveIntoHeld(src, dst string, allowExec bool) ([]Moved, error) {
	return deliver(src, dst, allowExec, true, true)
}

// CopyIntoHeld is CopyInto into a held folder (see MoveIntoHeld).
func CopyIntoHeld(src, dst string, allowExec bool) ([]Moved, error) {
	return deliver(src, dst, allowExec, false, true)
}

// CopyInto is MoveInto without removing anything from src. Every file is a
// real copy, never a hard link: it serves fan-out (one staging folder handed
// to several sessions), and each session must get its own copy that the
// others cannot change.
func CopyInto(src, dst string, allowExec bool) ([]Moved, error) {
	return deliver(src, dst, allowExec, false, false)
}

func deliver(src, dst string, allowExec, move, held bool) ([]Moved, error) {
	entries, err := os.ReadDir(src)
	if err != nil {
		return nil, err
	}
	dirMode := fs.FileMode(0o755)
	if held {
		dirMode = 0o700
	}
	if err := ensureDir(dst, dirMode); err != nil {
		return nil, err
	}
	if held {
		if err := InitHeld(dst); err != nil {
			return nil, err
		}
	}
	privList, srcHeld := privateNames(src)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int {
		if a.Name() < b.Name() {
			return -1
		}
		if a.Name() > b.Name() {
			return 1
		}
		return 0
	})
	var out []Moved
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), TempPrefix) {
			continue // staging holds only files we wrote; skip anything else, and uploads in progress
		}
		name, err := Name(e.Name())
		if err != nil {
			continue
		}
		st, err := e.Info()
		if err != nil {
			return out, err
		}
		from := filepath.Join(src, e.Name())
		exec := st.Mode()&0o111 != 0
		// Private: a secret-ish name, listed as private by a held source,
		// or (a source that is not held) already 0600 when it was written,
		// even if keep_names renamed it.
		isPrivate := func(final string) bool {
			return privList[e.Name()] || Secretish(final) || Secretish(e.Name()) ||
				(!srcHeld && st.Mode().Perm()&0o077 == 0)
		}
		// The mode is set before the file appears under its name (on the
		// source before a link, through the open file for a copy), never by
		// path afterwards: dst may be a workspace an agent writes in, and a
		// chmod by path would follow a symlink it put there in the
		// meantime.
		modeFor := func(final string) fs.FileMode {
			if held {
				return HeldMode(allowExec, exec)
			}
			mode := FileMode(final, e.Name(), allowExec, exec)
			if isPrivate(final) {
				mode &^= 0o077
			}
			return mode
		}
		final, err := placeNoClobber(from, dst, name, move, modeFor)
		if err != nil {
			return out, err
		}
		private := isPrivate(final)
		if held && private {
			if err := MarkPrivate(dst, final); err != nil {
				return out, err
			}
		}
		out = append(out, Moved{Name: final, Size: st.Size()})
	}
	if move {
		if err := os.RemoveAll(src); err != nil {
			return out, err
		}
	}
	return out, nil
}

// ensureDir creates dir (with mode) when missing and refuses a symlink or
// a file in its place.
func ensureDir(dir string, mode fs.FileMode) error {
	st, err := os.Lstat(dir)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(dir, mode); err != nil {
			return err
		}
		st, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a plain folder", dir)
	}
	return nil
}

// MoveFile moves the file from into dir under name, or the next free
// collision name, never replacing an existing file, and returns the name it
// got.
func MoveFile(from, dir, name string) (string, error) {
	return placeNoClobber(from, dir, name, true, nil)
}

// placeNoClobber puts from into dir under name or the next free collision
// name, never replacing an existing file: a hard link (or an exclusive
// create) fails when the name is taken, so two deliveries racing for one
// name both survive. A move links then removes the source; a copy (move
// false) always copies the bytes, so the two files never share an inode.
// modeFor (nil: keep from's bits) gives the mode for a candidate name; it
// is applied before the file is visible there (to from before a link, to
// the open copy otherwise).
func placeNoClobber(from, dir, name string, move bool, modeFor func(string) fs.FileMode) (string, error) {
	for tries := 0; tries < 100; tries++ {
		final, err := Unique(name, func(n string) bool {
			_, err := os.Lstat(filepath.Join(dir, n))
			return err == nil
		})
		if err != nil {
			return "", err
		}
		to := filepath.Join(dir, final)
		mode := fs.FileMode(0)
		if modeFor != nil {
			mode = modeFor(final)
		}
		if move {
			if modeFor != nil {
				if err := os.Chmod(from, mode); err != nil {
					return "", err
				}
			}
			err = os.Link(from, to)
			switch {
			case err == nil:
				_ = os.Remove(from)
				return final, nil
			case errors.Is(err, fs.ErrExist):
				continue // taken between the check and the link: next name
			}
		}
		// A copy, or no hard links here (another file system, FAT…): copy
		// into the name, created exclusively. A rename is never used: it
		// would replace a file that took the name a moment ago.
		if err := copyExclusive(from, to, mode); err != nil {
			if errors.Is(err, fs.ErrExist) {
				continue
			}
			return "", err
		}
		if move {
			_ = os.Remove(from)
		}
		return final, nil
	}
	return "", fmt.Errorf("no free name for %q", name)
}

// copyExclusive copies the regular file from to a new file to (which must
// not exist), with mode (0: from's permission bits). from must not be a
// symlink: what was opened must be what sits at from, so a symlink swapped
// in (from may be in a workspace) is never followed. The mode is set on the
// open file, never by path.
func copyExclusive(from, to string, mode fs.FileMode) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if lst, err := os.Lstat(from); err != nil || !st.Mode().IsRegular() || !os.SameFile(st, lst) {
		return fmt.Errorf("%s is not a regular file", from)
	}
	if mode == 0 {
		mode = st.Mode().Perm()
	}
	out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, cerr := io.Copy(out, in)
	if err := errors.Join(cerr, out.Chmod(mode), out.Close()); err != nil {
		_ = os.Remove(to)
		return err
	}
	return nil
}
