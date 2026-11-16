//go:build linux

package platform

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestCgroupRoomMiB(t *testing.T) {
	write := func(t *testing.T, path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const gib = 1 << 30
	setup := func(t *testing.T) (proc, cg, unit string) {
		root := t.TempDir()
		proc, cg = filepath.Join(root, "proc"), filepath.Join(root, "cg")
		write(t, filepath.Join(proc, "42", "cgroup"), "0::/user.slice/app.slice/lmstudio.service\n")
		unit = filepath.Join(cg, "user.slice", "app.slice", "lmstudio.service")
		return proc, cg, unit
	}
	num := func(n int64) string { return strconv.FormatInt(n, 10) + "\n" }

	t.Run("limit on the unit", func(t *testing.T) {
		proc, cg, unit := setup(t)
		write(t, filepath.Join(unit, "memory.max"), num(16*gib))
		write(t, filepath.Join(unit, "memory.current"), num(2*gib))
		write(t, filepath.Join(cg, "user.slice", "memory.max"), "max\n")
		write(t, filepath.Join(cg, "user.slice", "memory.current"), num(20*gib))
		if got := cgroupRoomMiB(proc, cg, 42); got != 14336 {
			t.Fatalf("room %d, want 14336", got)
		}
	})
	t.Run("tighter parent wins; page cache is not counted", func(t *testing.T) {
		proc, cg, unit := setup(t)
		write(t, filepath.Join(unit, "memory.max"), num(16*gib))
		write(t, filepath.Join(unit, "memory.current"), num(4*gib))
		write(t, filepath.Join(unit, "memory.stat"), "anon 1073741824\nfile 2147483648\n")
		app := filepath.Join(cg, "user.slice", "app.slice")
		write(t, filepath.Join(app, "memory.max"), num(10*gib))
		write(t, filepath.Join(app, "memory.current"), num(5*gib))
		// unit: 16 - (4 - 2) = 14 GiB; app.slice: 10 - 5 = 5 GiB.
		if got := cgroupRoomMiB(proc, cg, 42); got != 5*1024 {
			t.Fatalf("room %d, want 5120", got)
		}
	})
	t.Run("no limit anywhere", func(t *testing.T) {
		proc, cg, unit := setup(t)
		write(t, filepath.Join(unit, "memory.max"), "max\n")
		write(t, filepath.Join(unit, "memory.current"), num(gib))
		if got := cgroupRoomMiB(proc, cg, 42); got != -1 {
			t.Fatalf("room %d, want -1", got)
		}
	})
	t.Run("unknown pid or cgroup v1", func(t *testing.T) {
		proc, cg, _ := setup(t)
		if got := cgroupRoomMiB(proc, cg, 7); got != -1 {
			t.Fatalf("missing pid: %d", got)
		}
		write(t, filepath.Join(proc, "8", "cgroup"), "4:memory:/foo\n")
		if got := cgroupRoomMiB(proc, cg, 8); got != -1 {
			t.Fatalf("v1: %d", got)
		}
		if got := cgroupRoomMiB(proc, cg, 0); got != -1 {
			t.Fatalf("pid 0: %d", got)
		}
	})
}
