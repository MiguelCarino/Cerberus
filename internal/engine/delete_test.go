package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func TestLlamaCppDelete(t *testing.T) {
	dir := t.TempDir()
	scan := filepath.Join(dir, "scan")
	writeFile(t, filepath.Join(scan, "pub", "found.gguf"), 1<<20)
	writeFile(t, filepath.Join(scan, "pub", "other.gguf"), 1<<20)
	outside := filepath.Join(dir, "elsewhere", "big.gguf")
	writeFile(t, outside, 1<<20)
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = []string{scan}
	cfg.Models = []config.ModelConfig{{ID: "big", Path: outside}}
	b, _ := fakeLlama(t, cfg, "ok")
	if _, err := b.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(context.Background(), ModelInfo{ID: "found"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(scan, "pub", "found.gguf")); !errors.Is(err, os.ErrNotExist) {
		t.Error("found.gguf still there")
	}
	if _, err := os.Stat(filepath.Join(scan, "pub", "other.gguf")); err != nil {
		t.Error("a neighbour was deleted")
	}
	// configured with a path outside model_dirs: refused, file kept
	if err := b.Delete(context.Background(), ModelInfo{ID: "big"}); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("outside model_dirs: %v", err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("the file outside model_dirs was deleted")
	}
}

func TestLMStudioDelete(t *testing.T) {
	root := t.TempDir()
	gguf := filepath.Join(root, "DavidAU", "GLM-4.7-Flash-GGUF", "GLM-4.7-Flash-Q5_K_M.gguf")
	writeFile(t, gguf, 1<<20)
	mlx := filepath.Join(root, "qwen", "qwen3.8-27b")
	writeFile(t, filepath.Join(mlx, "model.safetensors"), 1<<20)
	b, _ := testLMStudio(t, "http://127.0.0.1:1/v1")
	b.roots = func() []string { return []string{root} }
	if err := b.Delete(context.Background(), ModelInfo{ID: "glm-4.7-flash", Name: "glm-4.7-flash"}); err != nil {
		t.Fatal(err)
	}
	// the file and the folders it emptied are gone; the root stays
	if _, err := os.Stat(filepath.Join(root, "DavidAU")); !errors.Is(err, os.ErrNotExist) {
		t.Error("emptied folders were kept")
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("the models root was removed")
	}
	// a model that is a folder (MLX) goes whole
	if err := b.Delete(context.Background(), ModelInfo{ID: "qwen/qwen3.8-27b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mlx); !errors.Is(err, os.ErrNotExist) {
		t.Error("folder model kept")
	}
	// files not on disk: refused
	if err := b.Delete(context.Background(), ModelInfo{ID: "text-embedding-nomic-embed-text-v1.5"}); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("missing files: %v", err)
	}
	if err := b.Delete(context.Background(), ModelInfo{ID: "nope"}); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("unknown model: %v", err)
	}
}

func TestRemoveInsideRefusesEscapes(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	os.MkdirAll(root, 0o755)
	secret := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(secret, []byte("x"), 0o600)
	if err := removeInside(root, filepath.Join(root, "..", "x"), true); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("..: %v", err)
	}
	if err := removeInside(root, root, true); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("the root itself: %v", err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	// a folder inside the root that is a symlink to elsewhere
	os.Symlink(filepath.Dir(secret), filepath.Join(root, "link"))
	if err := removeInside(root, filepath.Join(root, "link", "secret.txt"), false); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("through a symlinked folder: %v", err)
	}
	// a file symlink inside the root: the link goes, not its target
	os.Symlink(secret, filepath.Join(root, "file-link.gguf"))
	if err := removeInside(root, filepath.Join(root, "file-link.gguf"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Error("the symlink's target was deleted")
	}
}

func TestOllamaDelete(t *testing.T) {
	var got map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/delete" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		if got["model"] == "missing" {
			http.Error(w, `{"error":"model not found"}`, 404)
		}
	}))
	defer srv.Close()
	b := testOllama(srv.URL)
	if err := b.Delete(context.Background(), ModelInfo{ID: "llama3.2:3b", Name: "llama3.2:3b"}); err != nil || got["model"] != "llama3.2:3b" {
		t.Fatalf("%v %v", err, got)
	}
	if err := b.Delete(context.Background(), ModelInfo{ID: "missing", Name: "missing"}); err == nil {
		t.Error("a 404 was not reported")
	}
}

// deletingFake is a fakeBackend that can delete.
type deletingFake struct {
	*fakeBackend
	deleted []string
}

func (d *deletingFake) Delete(_ context.Context, m ModelInfo) error {
	d.deleted = append(d.deleted, m.ID)
	d.mu.Lock()
	for i, x := range d.models {
		if x.ID == m.ID {
			d.models = append(d.models[:i], d.models[i+1:]...)
			break
		}
	}
	d.mu.Unlock()
	return nil
}

func TestManagerDelete(t *testing.T) {
	ctx := context.Background()
	f := &deletingFake{fakeBackend: newFake("lmstudio", true, "a", "b")}
	cloud := newFake("openai", false, "p/x")
	m, _ := testManager(config.Defaults(), -1, f, cloud)
	_, rel, err := m.Acquire(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	// in use: refused
	if err := m.Delete(ctx, "a"); !errors.Is(err, ErrInUse) {
		t.Fatalf("in use: %v", err)
	}
	rel()
	// loaded but idle: unloaded, then deleted, and gone from the list
	if err := m.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if len(f.deleted) != 1 || len(f.unloaded()) != 1 {
		t.Errorf("deleted %v unloaded %v", f.deleted, f.unloaded())
	}
	for _, mi := range m.Models(ctx) {
		if mi.ID == "a" {
			t.Error("a is still listed")
		}
	}
	if err := m.Delete(ctx, "p/x"); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("cloud model: %v", err)
	}
	// a backend that cannot delete
	plain := newFake("ollama", true, "c")
	m2, _ := testManager(config.Defaults(), -1, plain)
	if err := m2.Delete(ctx, "c"); !errors.Is(err, ErrNotDeletable) {
		t.Errorf("no deleter: %v", err)
	}
}

// A model from LM Studio's catalogue: lms ls gives "qwen/qwen3.8-27b" as its
// path, which is a manifest folder in hub/models naming the repos that hold
// the files.
func TestLMStudioDeleteHubModel(t *testing.T) {
	root := filepath.Join(t.TempDir(), "models")
	hub := filepath.Join(t.TempDir(), "hub", "models")
	gguf := filepath.Join(root, "lmstudio-community", "Qwen3.8-27B-GGUF", "Qwen3.8-27B-Q4_K_M.gguf")
	writeFile(t, gguf, 1<<20)
	writeFile(t, filepath.Join(root, "lmstudio-community", "Qwen3.8-27B-GGUF", "mmproj-Qwen3.8-27B-BF16.gguf"), 1<<10)
	neighbour := filepath.Join(root, "lmstudio-community", "gemma-4-E4B-it-GGUF", "gemma.gguf")
	writeFile(t, neighbour, 1<<10)
	man := filepath.Join(hub, "qwen", "qwen3.8-27b")
	if err := os.MkdirAll(man, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(man, "manifest.json"), []byte(`{"type":"model","owner":"qwen","name":"qwen3.8-27b",
	  "dependencies":[{"type":"model","purpose":"baseModel","sources":[
	    {"type":"huggingface","user":"lmstudio-community","repo":"Qwen3.8-27B-GGUF"},
	    {"type":"huggingface","user":"lmstudio-community","repo":"Qwen3.8-27B-MLX-4bit"},
	    {"type":"huggingface","user":"..","repo":"x"}]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := testLMStudio(t, "http://127.0.0.1:1/v1")
	b.roots = func() []string { return []string{root} }
	b.hub = func() string { return hub }
	if err := b.Delete(context.Background(), ModelInfo{ID: "qwen/qwen3.8-27b"}); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{filepath.Dir(gguf), man} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s kept", gone)
		}
	}
	if _, err := os.Stat(neighbour); err != nil {
		t.Error("a neighbour repo was deleted")
	}
	if _, err := os.Stat(filepath.Join(hub, "qwen")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the emptied owner folder was kept")
	}
	if _, err := os.Stat(hub); err != nil {
		t.Error("the hub root was removed")
	}
}
