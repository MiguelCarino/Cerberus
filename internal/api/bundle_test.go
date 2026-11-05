package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/engine"
)

// bundleModels lists one model per engine; the llama.cpp file lies under
// the test's models folder.
type bundleModels struct {
	fakeModels
	gguf string
}

func (b *bundleModels) Models(context.Context) []engine.ModelInfo {
	return []engine.ModelInfo{
		{ID: "llama3.2:3b", Backend: "ollama", Name: "llama3.2:3b", Local: true, SizeMiB: 2000},
		{ID: "qwen-q4", Backend: "llamacpp", Name: "qwen-q4", Local: true, Path: b.gguf},
		{ID: "openrouter/big", Backend: "openai"},
	}
}

func TestConfigExportImportModels(t *testing.T) {
	e, _ := newConfigEnv(t)
	dir := t.TempDir()
	gguf := filepath.Join(dir, "Qwen", "Qwen3-GGUF", "qwen-q4.gguf")
	_ = os.MkdirAll(filepath.Dir(gguf), 0o700)
	_ = os.WriteFile(gguf, []byte("GGUF"), 0o600)
	e.srv.models = &bundleModels{gguf: gguf}
	if w := e.do("PATCH", "/api/config", `{"set":{"engines.llamacpp.model_dirs":["`+dir+`"]}}`, withBearer(testToken)); w.Code != 200 {
		t.Fatalf("PATCH %d %s", w.Code, w.Body)
	}

	// Export: every local model with a reference; no cloud model.
	w := e.do("GET", "/api/config/export", "", withBearer(testToken))
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Disposition"), "cerberus-settings-") {
		t.Fatalf("export %d %v %s", w.Code, w.Header(), w.Body)
	}
	if strings.Contains(w.Body.String(), testToken) {
		t.Fatal("export leaks the token")
	}
	b, err := config.ParseBundle(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	refs := map[string]string{}
	for _, m := range b.Models {
		refs[m.ID] = m.Ref
	}
	if len(refs) != 2 || refs["llama3.2:3b"] != "llama3.2:3b" || refs["qwen-q4"] != "https://huggingface.co/Qwen/Qwen3-GGUF/resolve/main/qwen-q4.gguf" {
		t.Fatalf("models %+v", b.Models)
	}
	// A source note wins over the guessed link.
	_ = os.WriteFile(gguf+downloads.SourceSuffix, []byte("https://huggingface.co/Qwen/Qwen3-GGUF/resolve/v2/q/qwen-q4.gguf"), 0o600)
	w = e.do("GET", "/api/config/export?sections=models", "", withBearer(testToken))
	if !strings.Contains(w.Body.String(), "resolve/v2/q/qwen-q4.gguf") || strings.Contains(w.Body.String(), `"agents"`) {
		t.Fatalf("models-only export %s", w.Body)
	}
	if w := e.do("GET", "/api/config/export?sections=nope", "", withBearer(testToken)); w.Code != 400 {
		t.Fatalf("unknown section %d", w.Code)
	}

	// Import on a machine that has none of them: a registry entry with a
	// path from the other machine is pointed at where the download lands.
	bundle := `{"version":1,"kind":"cerberus-profile","models":[
	  {"id":"llama3.2:3b","backend":"ollama","name":"llama3.2:3b","ref":"llama3.2:3b"},
	  {"id":"mistral:7b","backend":"ollama","ref":"mistral:7b"},
	  {"id":"far","backend":"llamacpp","ref":"https://huggingface.co/a/b/resolve/main/far.gguf",
	   "config":{"id":"far","backend":"llamacpp","path":"/other/machine/far.gguf","ctx":4096}}]}`
	w = e.do("POST", "/api/config/import", `{"apply":false,"bundle":`+bundle+`}`, withBearer(testToken))
	if w.Code != 200 {
		t.Fatalf("preview %d %s", w.Code, w.Body)
	}
	var rep ImportReply
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if rep.Applied || len(rep.Changes) != 1 || rep.Changes[0].Path != "models" || len(rep.Sections) != 1 {
		t.Fatalf("preview %+v", rep)
	}
	if len(e.srv.conf().Models) != 0 {
		t.Fatal("preview changed the config")
	}
	w = e.do("POST", "/api/config/import", `{"apply":true,"bundle":`+bundle+`}`, withBearer(testToken))
	rep = ImportReply{}
	_ = json.Unmarshal(w.Body.Bytes(), &rep)
	if w.Code != 200 || !rep.Applied {
		t.Fatalf("apply %d %s", w.Code, w.Body)
	}
	st := map[string]ImportModel{}
	for _, m := range rep.Models {
		st[m.ID] = m
	}
	if !st["llama3.2:3b"].Present || st["mistral:7b"].Present || st["far"].Present || !st["far"].Registered || st["mistral:7b"].Registered {
		t.Fatalf("models %+v", rep.Models)
	}
	ms := e.srv.conf().Models
	if len(ms) != 1 || ms[0].Path != filepath.Join(dir, "a", "b", "far.gguf") || ms[0].Ctx != 4096 {
		t.Fatalf("registry %+v", ms)
	}
	if m := diskConfig(t, e.srv.cfg.Path())["models"]; m == nil {
		t.Fatal("import not saved")
	}

	// Remove from the registry.
	w = e.do("POST", "/api/config/models/forget", `{"ids":["far","unknown"]}`, withBearer(testToken))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"far"`) || len(e.srv.conf().Models) != 0 {
		t.Fatalf("forget %d %s %+v", w.Code, w.Body, e.srv.conf().Models)
	}
	if w := e.do("POST", "/api/config/models/forget", `{"ids":[]}`, withBearer(testToken)); w.Code != 400 {
		t.Fatalf("empty forget %d", w.Code)
	}

	// Not a bundle.
	for _, body := range []string{`{"bundle":{"kind":"other","version":1}}`, `{"bundle":{"kind":"cerberus-profile","version":99}}`, `{"bundle":"x"}`} {
		if w := e.do("POST", "/api/config/import", body, withBearer(testToken)); w.Code != 400 {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body)
		}
	}
}
