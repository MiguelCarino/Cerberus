package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestHelperProcess is not a real test: the llama.cpp tests re-exec the test
// binary as a fake llama-server (see fakeLlamaCommand).
func TestHelperProcess(t *testing.T) {
	if os.Getenv("CERBERUS_FAKE_LLAMA") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	if os.Getenv("CERBERUS_FAKE_LLAMA_MODE") == "exit" {
		fmt.Println("fake llama-server: model file is corrupt")
		os.Exit(3)
	}
	port := ""
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--port" {
			port = args[i+1]
		}
	}
	start := time.Now()
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		if time.Since(start) < 150*time.Millisecond {
			w.WriteHeader(http.StatusServiceUnavailable) // "loading model"
			return
		}
		fmt.Fprint(w, `{"status":"ok"}`)
	})
	fmt.Println("fake llama-server listening on", port)
	_ = http.ListenAndServe("127.0.0.1:"+port, mux)
	os.Exit(0)
}

// fakeLlama returns a llamacpp backend whose binary is the test binary.
func fakeLlama(t *testing.T, cfg *config.Config, mode string) (*llamaCpp, string) {
	t.Helper()
	logs := t.TempDir()
	b := newLlamaCpp(cfg, quietLog())
	b.lookPath = func(string) (string, error) { return "/fake/llama-server", nil }
	b.logDir = func() (string, error) { return logs, nil }
	b.command = func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestHelperProcess$", "--"}, args...)...)
		cmd.Env = append(os.Environ(), "CERBERUS_FAKE_LLAMA=1", "CERBERUS_FAKE_LLAMA_MODE="+mode)
		return cmd
	}
	return b, logs
}

func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanGGUF(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "plain.gguf"), 3<<20)
	writeFile(t, filepath.Join(root, "notes.txt"), 10)
	writeFile(t, filepath.Join(root, "vis", "gemma.gguf"), 1<<20)
	writeFile(t, filepath.Join(root, "vis", "mmproj-gemma-f16.gguf"), 1<<20)
	writeFile(t, filepath.Join(root, "split", "big-00001-of-00003.gguf"), 2<<20)
	writeFile(t, filepath.Join(root, "split", "big-00002-of-00003.gguf"), 2<<20)
	writeFile(t, filepath.Join(root, "a", "b", "deep.gguf"), 10)
	writeFile(t, filepath.Join(root, "a", "b", "c", "toodeep.gguf"), 10)

	got := map[string]ggufFile{}
	for _, f := range scanGGUF([]string{root, filepath.Join(root, "missing")}) {
		got[f.ID] = f
	}
	want := []string{"big", "deep", "gemma", "plain"}
	var ids []string
	for id := range got {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	if !slices.Equal(ids, want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	if got["plain"].SizeMiB != 3 || got["big"].SizeMiB != 6 {
		t.Errorf("sizes: plain %d big %d", got["plain"].SizeMiB, got["big"].SizeMiB)
	}
	if !strings.HasSuffix(got["gemma"].MMProj, "mmproj-gemma-f16.gguf") || got["plain"].MMProj != "" {
		t.Errorf("mmproj: gemma %q plain %q", got["gemma"].MMProj, got["plain"].MMProj)
	}
}

func TestLlamaListAndArgs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "scan", "found.gguf"), 2<<20)
	cfgPath := filepath.Join(dir, "cfg", "big.gguf")
	writeFile(t, cfgPath, 5<<20)

	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = []string{filepath.Join(dir, "scan")}
	cfg.Engines.LlamaCpp.Args = []string{"-ngl", "99"}
	cfg.Models = []config.ModelConfig{
		{ID: "big", Path: cfgPath, Ctx: 8192, Parallel: 2, Draft: "/m/draft.gguf", Args: []string{"--temp", "0.2"}},
		{ID: "sized", Path: filepath.Join(dir, "nofile.gguf"), SizeMiB: 777},
		{ID: "lms-only", Backend: "lmstudio", Path: "/x.gguf"}, // not ours
	}
	b, _ := fakeLlama(t, cfg, "ok")
	ms, err := b.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range ms {
		byID[m.ID] = m
	}
	if len(ms) != 3 || byID["big"].SizeMiB != 5 || byID["sized"].SizeMiB != 777 || byID["found"].SizeMiB != 2 {
		t.Fatalf("list = %+v", ms)
	}
	if byID["big"].Ctx != 8192 || byID["found"].Ctx != 32768 || !byID["found"].Local {
		t.Errorf("ctx/local: %+v", ms)
	}

	got := strings.Join(b.args(b.specs["big"], 39250), " ")
	want := "-m " + cfgPath + " --host 127.0.0.1 --port 39250 -c 8192 -np 2 --jinja --flash-attn on --metrics -md /m/draft.gguf -ngl 99 --temp 0.2"
	if got != want {
		t.Errorf("args\n got %s\nwant %s", got, want)
	}
}

func TestLlamaLoadUnload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.gguf")
	writeFile(t, path, 1024)
	// Occupy PortBase so the backend must skip it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	base := l.Addr().(*net.TCPAddr).Port

	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = nil
	cfg.Engines.LlamaCpp.PortBase = base
	cfg.Engines.LoadTimeoutSec = 20
	cfg.Models = []config.ModelConfig{{ID: "org/m", Path: path}}
	b, logs := fakeLlama(t, cfg, "ok")
	ctx := context.Background()
	if _, err := b.List(ctx); err != nil {
		t.Fatal(err)
	}
	info := ModelInfo{ID: "org/m", Name: "org/m", Local: true}

	ep, err := b.Load(ctx, info)
	if err != nil {
		t.Fatal(err)
	}
	if ep.BaseURL == "" || strings.HasSuffix(ep.BaseURL, ":"+strconv.Itoa(base)+"/v1") || !ep.Local {
		t.Fatalf("endpoint %+v (port base %d must be skipped)", ep, base)
	}
	ep2, err := b.Load(ctx, info) // idempotent
	if err != nil || ep2.BaseURL != ep.BaseURL {
		t.Fatalf("second load: %v %+v", err, ep2)
	}
	if ms, _ := b.List(ctx); !ms[0].Loaded {
		t.Fatal("not listed as loaded")
	}
	raw, err := os.ReadFile(filepath.Join(logs, "org_m.log"))
	if err != nil || !strings.Contains(string(raw), "--flash-attn on") || !strings.Contains(string(raw), "listening") {
		t.Fatalf("log: %v %q", err, raw)
	}

	if err := b.Unload(ctx, info); err != nil {
		t.Fatal(err)
	}
	if b.Endpoint(info).BaseURL != "" {
		t.Fatal("endpoint still set after unload")
	}
	if getOK(ctx, http.DefaultClient, strings.TrimSuffix(ep.BaseURL, "/v1")+"/health", nil) {
		t.Fatal("server still answering after unload")
	}
}

func TestLlamaLoadFailsWhenProcessExits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.gguf")
	writeFile(t, path, 10)
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = nil
	cfg.Models = []config.ModelConfig{{ID: "bad", Path: path}}
	b, _ := fakeLlama(t, cfg, "exit")
	start := time.Now()
	_, err := b.Load(context.Background(), ModelInfo{ID: "bad", Local: true})
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("err = %v, want exit error with log tail", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("did not notice the exit promptly")
	}
}

func TestLlamaStopAll(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.ModelDirs = []string{dir}
	writeFile(t, filepath.Join(dir, "one.gguf"), 10)
	writeFile(t, filepath.Join(dir, "two.gguf"), 10)
	b, _ := fakeLlama(t, cfg, "ok")
	ctx := context.Background()
	for _, id := range []string{"one", "two"} {
		if _, err := b.Load(ctx, ModelInfo{ID: id, Local: true}); err != nil {
			t.Fatal(err)
		}
	}
	sctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	b.StopAll(sctx)
	ms, _ := b.List(ctx)
	for _, m := range ms {
		if m.Loaded {
			t.Fatalf("%s still loaded", m.ID)
		}
	}
}

func TestLlamaUnavailableWithoutBinary(t *testing.T) {
	cfg := config.Defaults()
	cfg.Engines.LlamaCpp.Binary = "definitely-not-a-real-llama-server"
	if newLlamaCpp(cfg, quietLog()).Available() {
		t.Fatal("available without binary")
	}
	cfg.Engines.LlamaCpp.Enabled = false
	b := newLlamaCpp(cfg, quietLog())
	b.lookPath = func(string) (string, error) { return "/x", nil }
	if b.Available() {
		t.Fatal("available while disabled")
	}
}
