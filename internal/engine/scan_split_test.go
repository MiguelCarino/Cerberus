package engine

import (
	"bytes"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// A split set with a shard missing is not listed, and the debug log names
// the missing files.
func TestScanGGUFIncompleteSetLogged(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "o", "r", "m-Q4-00001-of-00003.gguf"), 1<<20)
	writeFile(t, filepath.Join(root, "o", "r", "m-Q4-00003-of-00003.gguf"), 1<<20)
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if got := scanGGUF([]string{root}, log); len(got) != 0 {
		t.Fatalf("an incomplete set was listed: %+v", got)
	}
	if s := buf.String(); !strings.Contains(s, "split model left out") || !strings.Contains(s, "m-Q4-00002-of-00003.gguf") ||
		strings.Contains(s, "m-Q4-00003-of-00003.gguf,") {
		t.Fatalf("log %q", s)
	}
	// Once the last shard lands, the set is one model of the summed size.
	writeFile(t, filepath.Join(root, "o", "r", "m-Q4-00002-of-00003.gguf"), 2<<20)
	got := scanGGUF([]string{root}, log)
	if len(got) != 1 || got[0].ID != "m-Q4" || got[0].SizeMiB != 4 || !strings.HasSuffix(got[0].Path, "m-Q4-00001-of-00003.gguf") {
		t.Fatalf("complete set %+v", got)
	}
}
