package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestShellAssetsEmbedded checks that every local file the shell and the
// scripts reference is in the embedded tree, so a rename can't ship a
// broken UI.
func TestShellAssetsEmbedded(t *testing.T) {
	ui, err := fs.Sub(FS, "dist")
	if err != nil {
		t.Fatal(err)
	}
	index, err := fs.ReadFile(ui, "index.html")
	if err != nil {
		t.Fatalf("index.html: %v", err)
	}
	refs := regexp.MustCompile(`(?:href|src)="(assets/[^"]+)"`).FindAllSubmatch(index, -1)
	if len(refs) < 4 {
		t.Fatalf("index.html references only %d assets", len(refs))
	}
	for _, m := range refs {
		if _, err := fs.Stat(ui, string(m[1])); err != nil {
			t.Errorf("index.html references missing %s", m[1])
		}
	}

	imports := regexp.MustCompile(`(?m)^\s*import\s[^'"]*['"](\.[^'"]+)['"]`)
	err = fs.WalkDir(ui, "assets/js", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".js") {
			return err
		}
		src, err := fs.ReadFile(ui, p)
		if err != nil {
			return err
		}
		for _, m := range imports.FindAllSubmatch(src, -1) {
			target := path.Join(path.Dir(p), string(m[1]))
			if _, err := fs.Stat(ui, target); err != nil {
				t.Errorf("%s imports missing %s", p, target)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestVendorFiles(t *testing.T) {
	for _, p := range []string{
		"dist/assets/vendor/xterm/xterm.mjs",
		"dist/assets/vendor/xterm/xterm.css",
		"dist/assets/vendor/xterm/LICENSE",
		"dist/assets/vendor/addon-fit/addon-fit.mjs",
		"dist/assets/vendor/addon-fit/LICENSE",
		"dist/assets/vendor/VERSIONS.txt",
		"dist/assets/fonts/OFL.txt",
	} {
		if _, err := fs.Stat(FS, p); err != nil {
			t.Errorf("missing %s", p)
		}
	}
}

// TestServiceWorkerBuild checks that sw.js lists the current app shell and
// carries its build hash, with the rules of web/tools/build_sw.py, so a
// changed asset always means a new cache (and "Reload to update").
func TestServiceWorkerBuild(t *testing.T) {
	ui, err := fs.Sub(FS, "dist")
	if err != nil {
		t.Fatal(err)
	}
	entries := map[string]string{
		"/":                     "index.html",
		"/manifest.webmanifest": "manifest.webmanifest",
		"/carino-clock.js":      "assets/carino-clock.js",
	}
	exts := map[string]bool{".js": true, ".mjs": true, ".css": true, ".woff2": true, ".png": true, ".svg": true}
	err = fs.WalkDir(ui, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !exts[path.Ext(p)] {
			return err
		}
		entries["/"+p] = p
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	urls := make([]string, 0, len(entries))
	for u := range entries {
		urls = append(urls, u)
	}
	sort.Strings(urls)
	h := sha256.New()
	for _, u := range urls {
		raw, err := fs.ReadFile(ui, entries[u])
		if err != nil {
			t.Fatalf("shell file %s: %v", u, err)
		}
		sum := sha256.Sum256(raw)
		fmt.Fprintf(h, "%s\n%s\n", u, hex.EncodeToString(sum[:]))
	}
	want := hex.EncodeToString(h.Sum(nil))[:12]

	sw, err := fs.ReadFile(ui, "sw.js")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`const BUILD = '([0-9a-f]+)';`).FindSubmatch(sw)
	if m == nil {
		t.Fatal("sw.js has no BUILD")
	}
	listed := regexp.MustCompile(`(?s)const SHELL = \[(.*?)\];`).FindSubmatch(sw)
	if listed == nil {
		t.Fatal("sw.js has no SHELL")
	}
	var got []string
	for _, q := range regexp.MustCompile(`'([^']+)'`).FindAllSubmatch(listed[1], -1) {
		got = append(got, string(q[1]))
	}
	if string(m[1]) != want || strings.Join(got, ",") != strings.Join(urls, ",") {
		t.Fatalf("sw.js is stale (BUILD %s, want %s; %d shell files listed, want %d): run python3 web/tools/build_sw.py",
			m[1], want, len(got), len(urls))
	}
	for _, u := range got {
		if strings.HasPrefix(u, "/api") || strings.HasPrefix(u, "/v1") {
			t.Errorf("shell lists %s", u)
		}
	}
}

// TestPWAFiles checks the manifest and the icons it names.
func TestPWAFiles(t *testing.T) {
	raw, err := fs.ReadFile(FS, "dist/manifest.webmanifest")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Name  string
		Icons []struct{ Src, Sizes, Type, Purpose string }
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	if m.Name != "Cerberus" || len(m.Icons) < 3 {
		t.Fatalf("manifest: %+v", m)
	}
	for _, ic := range m.Icons {
		if _, err := fs.Stat(FS, path.Join("dist", ic.Src)); err != nil {
			t.Errorf("manifest icon %s missing", ic.Src)
		}
	}
}
