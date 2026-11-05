package config

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestBundleModelsExport(t *testing.T) {
	c := Defaults()
	c.Models = []ModelConfig{
		{ID: "qwen-gguf", Backend: "llamacpp", Path: "/models/a/b/qwen.gguf", Ctx: 8192, Args: []string{"--flash-attn", "HF_TOKEN=abc"}},
		{ID: "gone", Path: "/models/gone.gguf"},
	}
	inv := []BundleModel{
		{ID: "llama3.2:3b", Backend: "ollama", Name: "llama3.2:3b", Ref: "llama3.2:3b", SizeMiB: 2000},
		{ID: "qwen-gguf", Backend: "llamacpp", Ref: "https://huggingface.co/a/b/resolve/main/qwen.gguf", Config: &ModelConfig{ID: "ignored"}},
		{ID: "llama3.2:3b", Backend: "ollama"}, // duplicate
		{ID: ""},
	}
	raw, err := c.Export(ExportOptions{Sections: []string{SectionModels}, Inventory: inv})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "abc") {
		t.Fatalf("credential arg exported:\n%s", raw)
	}
	b, err := ParseBundle(raw)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, m := range b.Models {
		ids = append(ids, m.ID)
	}
	if !slices.Equal(ids, []string{"llama3.2:3b", "qwen-gguf", "gone"}) {
		t.Fatalf("ids %v", ids)
	}
	if b.Models[0].Config != nil || b.Models[0].Ref != "llama3.2:3b" {
		t.Fatalf("ollama model %+v", b.Models[0])
	}
	if mc := b.Models[1].Config; mc == nil || mc.ID != "qwen-gguf" || mc.Ctx != 8192 || !slices.Equal(mc.Args, []string{"--flash-attn"}) {
		t.Fatalf("configured model %+v", b.Models[1].Config)
	}
	if b.Models[2].Backend != "llamacpp" || b.Models[2].Config == nil {
		t.Fatalf("config-only model %+v", b.Models[2])
	}
	if !slices.Contains(b.Omitted, "models[1].config.args") {
		t.Fatalf("omitted %v", b.Omitted)
	}
	// The source config is untouched.
	if len(c.Models[0].Args) != 2 {
		t.Fatalf("export changed the config: %v", c.Models[0].Args)
	}
}

func TestBundleModelsImport(t *testing.T) {
	b := Bundle{Version: BundleVersion, Kind: BundleKind, Models: []BundleModel{
		{ID: "llama3.2:3b", Backend: "ollama", Ref: "llama3.2:3b"},
		{ID: "qwen-gguf", Backend: "llamacpp", Ref: "https://huggingface.co/a/b/resolve/main/qwen.gguf",
			Config: &ModelConfig{ID: "qwen-gguf", Backend: "llamacpp", Path: "/elsewhere/qwen.gguf", Ctx: 4096}},
		{ID: "here", Backend: "llamacpp", Config: &ModelConfig{ID: "here", Path: "/present/here.gguf"}},
	}}
	raw, _ := json.Marshal(b)
	c := Defaults()
	c.Models = []ModelConfig{{ID: "here", Path: "/old/here.gguf"}, {ID: "keep", Path: "/present/keep.gguf"}}
	opts := ImportOptions{
		Exists: func(p string) bool { return strings.HasPrefix(p, "/present/") },
		Place: func(m BundleModel) string {
			if m.ID == "qwen-gguf" {
				return "/models/a/b/qwen.gguf"
			}
			return ""
		},
	}
	changes, apply, err := c.ImportWith(raw, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 1 || changes[0].Path != "models" {
		t.Fatalf("changes %+v", changes)
	}
	if err := apply(); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range c.Models {
		got[m.ID] = m.Path
	}
	want := map[string]string{"here": "/present/here.gguf", "keep": "/present/keep.gguf", "qwen-gguf": "/models/a/b/qwen.gguf"}
	if len(got) != len(want) {
		t.Fatalf("models %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("models %v, want %v", got, want)
		}
	}
	// Importing the same bundle again changes nothing.
	if changes, _, err := c.ImportWith(raw, opts); err != nil || len(changes) != 0 {
		t.Fatalf("second import: %v %+v", err, changes)
	}
}
