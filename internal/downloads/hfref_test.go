package downloads

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHFRefHelpers(t *testing.T) {
	dest, err := HFDest("/m", "https://hf.co/Qwen/Qwen3-GGUF/blob/main/sub/q4.gguf?x=1")
	if err != nil || dest != filepath.Join("/m", "Qwen", "Qwen3-GGUF", "q4.gguf") {
		t.Fatalf("HFDest = %q, %v", dest, err)
	}
	if _, err := HFDest("", "https://hf.co/a/b/resolve/main/x.gguf"); err == nil {
		t.Fatal("HFDest without a folder")
	}
	if _, err := HFDest("/m", "https://example.com/a/b/resolve/main/x.gguf"); err == nil {
		t.Fatal("HFDest accepted another host")
	}
	if g := HFGuess("Qwen/Qwen3-GGUF/q4.gguf"); g != "https://huggingface.co/Qwen/Qwen3-GGUF/resolve/main/q4.gguf" {
		t.Fatalf("HFGuess = %q", g)
	}
	for _, rel := range []string{"q4.gguf", "a/q4.gguf", "a/b/c/q4.gguf", "a/b/q4.bin", "../b/q4.gguf"} {
		if g := HFGuess(rel); g != "" {
			t.Fatalf("HFGuess(%q) = %q", rel, g)
		}
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "q4.gguf")
	if HFSource(f) != "" {
		t.Fatal("source without a note")
	}
	_ = os.WriteFile(f+SourceSuffix, []byte("https://hf.co/a/b/resolve/v2/sub/q4.gguf\n"), 0o600)
	if s := HFSource(f); s != "https://huggingface.co/a/b/resolve/v2/sub/q4.gguf" {
		t.Fatalf("HFSource = %q", s)
	}
	_ = os.WriteFile(f+SourceSuffix, []byte("https://evil.example/x.gguf"), 0o600)
	if s := HFSource(f); s != "" {
		t.Fatalf("HFSource accepted %q", s)
	}
}
