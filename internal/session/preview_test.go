package session

import (
	"os"
	"path/filepath"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func writeFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDetectStaticOrder: the root first, then public, dist, build, out,
// site, _site, docs; a folder without index.html does not count.
func TestDetectStaticOrder(t *testing.T) {
	wt, _ := filepath.EvalSymlinks(t.TempDir())
	if got := detectStatic(wt); got != "" {
		t.Fatalf("empty worktree: %q", got)
	}
	writeFile(t, filepath.Join(wt, "docs", "index.html"), "d")
	writeFile(t, filepath.Join(wt, "dist", "app.js"), "x") // no index.html
	if got := detectStatic(wt); got != filepath.Join(wt, "docs") {
		t.Fatalf("docs only: %q", got)
	}
	writeFile(t, filepath.Join(wt, "dist", "index.html"), "d")
	if got := detectStatic(wt); got != filepath.Join(wt, "dist") {
		t.Fatalf("dist before docs: %q", got)
	}
	writeFile(t, filepath.Join(wt, "public", "index.html"), "p")
	if got := detectStatic(wt); got != filepath.Join(wt, "public") {
		t.Fatalf("public before dist: %q", got)
	}
	writeFile(t, filepath.Join(wt, "index.html"), "r")
	if got := detectStatic(wt); got != wt {
		t.Fatalf("root first: %q", got)
	}
	// A folder named index.html is not a page.
	wt2, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(wt2, "index.html"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := detectStatic(wt2); got != "" {
		t.Fatalf("index.html folder: %q", got)
	}
}

// TestStaticConfigWins: a configured preview dir is served even when the
// root has an index.html; a command-only config still detects.
func TestStaticConfigWins(t *testing.T) {
	wt, _ := filepath.EvalSymlinks(t.TempDir())
	writeFile(t, filepath.Join(wt, "index.html"), "r")
	writeFile(t, filepath.Join(wt, "site", "page.html"), "s")
	m := &Manager{}
	m.live.Store(config.Defaults())
	writeFile(t, filepath.Join(wt, ".cerberus.json"), `{"preview":{"dir":"site"}}`)
	if got := m.staticDir("", wt); got != filepath.Join(wt, "site") {
		t.Fatalf("config dir: %q", got)
	}
	writeFile(t, filepath.Join(wt, ".cerberus.json"), `{"preview":{"command":"npm run dev"}}`)
	if got := m.staticDir("", wt); got != wt {
		t.Fatalf("command only: %q", got)
	}
	if got := m.staticDir("", ""); got != "" {
		t.Fatalf("no worktree: %q", got)
	}
}

// TestPackageCommand: the script and tool come from package.json and the
// lockfile; known dev servers get the port flags.
func TestPackageCommand(t *testing.T) {
	cases := []struct {
		name, pkg, lock, want, src string
	}{
		{"vite npm", `{"scripts":{"dev":"vite","start":"node s.js"}}`, "", `npm run dev -- --port {port} --strictPort --host 127.0.0.1`, `package.json "dev"`},
		{"vite pnpm", `{"scripts":{"dev":"vite"}}`, "pnpm-lock.yaml", `pnpm run dev --port {port} --strictPort --host 127.0.0.1`, `package.json "dev"`},
		{"next yarn", `{"scripts":{"dev":"next dev"}}`, "yarn.lock", `yarn run dev -p {port} -H 127.0.0.1`, `package.json "dev"`},
		{"start only (PORT env)", `{"scripts":{"start":"node server.js"}}`, "bun.lock", `bun run start`, `package.json "start"`},
		{"preview last", `{"scripts":{"build":"x","preview":"astro preview"}}`, "", `npm run preview -- --port {port} --host 127.0.0.1`, `package.json "preview"`},
		{"nothing", `{"scripts":{"build":"x"}}`, "", "", ""},
		{"bad json", `{`, "", "", ""},
	}
	for _, c := range cases {
		wt := t.TempDir()
		writeFile(t, filepath.Join(wt, "package.json"), c.pkg)
		if c.lock != "" {
			writeFile(t, filepath.Join(wt, c.lock), "")
		}
		got, src := packageCommand(wt)
		if got != c.want || src != c.src {
			t.Errorf("%s: got %q (%q), want %q (%q)", c.name, got, src, c.want, c.src)
		}
	}
	if got, _ := packageCommand(t.TempDir()); got != "" {
		t.Errorf("no package.json: %q", got)
	}
}

// TestPreviewCommandConfigWins: .cerberus.json's command beats package.json.
func TestPreviewCommandConfigWins(t *testing.T) {
	wt := t.TempDir()
	writeFile(t, filepath.Join(wt, "package.json"), `{"scripts":{"dev":"vite"}}`)
	writeFile(t, filepath.Join(wt, ".cerberus.json"), `{"preview":{"command":"./serve {port}"}}`)
	m := &Manager{}
	m.live.Store(config.Defaults())
	got, src := m.previewCommand("", wt)
	if got != "./serve {port}" || src != ".cerberus.json" {
		t.Fatalf("got %q from %q", got, src)
	}
}
