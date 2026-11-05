package vault

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"carino.systems/cerberus/internal/config"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func testConfig(t *testing.T, backend string) (*config.Config, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.SetPath(filepath.Join(dir, "config.json"))
	cfg.Vault.Backend = backend
	return cfg, dir
}

func checkMode(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("%s mode %v, want 0600", filepath.Base(path), st.Mode().Perm())
	}
}

func TestFileRoundTrip(t *testing.T) {
	cfg, dir := testConfig(t, "file")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if v.Backend() != BackendFile {
		t.Fatalf("backend %q", v.Backend())
	}
	const secret = "sk-very-secret-value-123"
	if err := v.Set("openai", secret, "work key"); err != nil {
		t.Fatal(err)
	}
	if err := v.Set("short", "abc", ""); err != nil {
		t.Fatal(err)
	}
	if got, err := v.Get("openai"); err != nil || got != secret {
		t.Fatalf("get %q %v", got, err)
	}
	if _, err := v.Get("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	checkMode(t, filepath.Join(dir, "vault.key"))
	checkMode(t, filepath.Join(dir, "vault.json"))
	raw, _ := os.ReadFile(filepath.Join(dir, "vault.json"))
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("plaintext value in vault.json")
	}

	// Reopen from disk: same values, a fresh nonce per write.
	v2, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Get("openai"); err != nil || got != secret {
		t.Fatalf("reopened get %q %v", got, err)
	}
	var doc fileDoc
	json.Unmarshal(raw, &doc)
	before := doc.Entries["openai"].Value
	if err := v2.Set("openai", secret, "work key"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "vault.json"))
	json.Unmarshal(raw, &doc)
	if doc.Entries["openai"].Value == before {
		t.Error("same ciphertext twice: nonce reused")
	}

	if vals := v2.Values(); len(vals) != 2 || vals[0] != secret || vals[1] != "abc" {
		t.Errorf("values %v", vals)
	}
	if err := v2.Delete("short"); err != nil {
		t.Fatal(err)
	}
	if err := v2.Delete("short"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	// A deleted value stays masked (running sessions may still hold it).
	if vals := v2.Values(); len(vals) != 2 || vals[0] != secret || vals[1] != "abc" {
		t.Errorf("values after delete %v", vals)
	}
	if err := v2.Set("new-one", "zzzzzzz", ""); err != nil {
		t.Fatal(err)
	}
	if vals := v2.Values(); len(vals) != 3 {
		t.Errorf("values after set %v", vals)
	}
}

func TestValidation(t *testing.T) {
	cfg, _ := testConfig(t, "file")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"", "has space", "slash/x", strings.Repeat("a", 65), "ü"} {
		if err := v.Set(n, "x", ""); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: %v", n, err)
		}
	}
	if err := v.Set(strings.Repeat("a", 64), "x", ""); err != nil {
		t.Errorf("64-char name: %v", err)
	}
	if err := v.Set("ok", "", ""); !errors.Is(err, ErrValueSize) {
		t.Errorf("empty: %v", err)
	}
	if err := v.Set("ok", strings.Repeat("x", MaxValueBytes+1), ""); !errors.Is(err, ErrValueSize) {
		t.Errorf("oversize: %v", err)
	}
	if err := v.Set("ok", strings.Repeat("x", MaxValueBytes), ""); err != nil {
		t.Errorf("16 KiB: %v", err)
	}
}

func TestWrongKeyDetected(t *testing.T) {
	cfg, dir := testConfig(t, "file")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("a", "value-a", ""); err != nil {
		t.Fatal(err)
	}
	other := make([]byte, keySize)
	rand.Read(other)
	if err := os.WriteFile(filepath.Join(dir, "vault.key"), other, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg, quietLog()); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("wrong key: %v", err)
	}
	os.Remove(filepath.Join(dir, "vault.key"))
	if _, err := Open(cfg, quietLog()); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("missing key: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.key")); err == nil {
		t.Error("a new key was created over an existing vault")
	}
}

func TestMigrationFromSecretsJSON(t *testing.T) {
	cfg, dir := testConfig(t, "file")
	// A vault value that secrets.json must not clobber.
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Set("kept", "vault-wins", ""); err != nil {
		t.Fatal(err)
	}
	legacy := `{"anthropic":"sk-ant-legacy","kept":"old","bad name":"x"}`
	if err := os.WriteFile(filepath.Join(dir, "secrets.json"), []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err = Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := v.Get("anthropic"); got != "sk-ant-legacy" {
		t.Errorf("imported %q", got)
	}
	if got, _ := v.Get("kept"); got != "vault-wins" {
		t.Errorf("kept %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.json")); !os.IsNotExist(err) {
		t.Errorf("secrets.json still there: %v", err)
	}
	checkMode(t, filepath.Join(dir, "secrets.json.migrated"))
	items, _ := v.List()
	for _, it := range items {
		if it.Name == "anthropic" && (it.Note != "imported from secrets.json" || it.Length != len("sk-ant-legacy")) {
			t.Errorf("item %+v", it)
		}
	}
	// Opening again does not re-import anything.
	if _, err := Open(cfg, quietLog()); err != nil {
		t.Fatal(err)
	}
}

func TestListNeverReturnsValues(t *testing.T) {
	cfg, _ := testConfig(t, "file")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	const secret = "ghp_abcdefghijklmnop"
	if err := v.Set("github", secret, "repo token"); err != nil {
		t.Fatal(err)
	}
	items, err := v.List()
	if err != nil || len(items) != 1 {
		t.Fatalf("list %+v %v", items, err)
	}
	it := items[0]
	if it.Name != "github" || it.Note != "repo token" || it.Length != len(secret) || it.Updated.IsZero() {
		t.Errorf("item %+v", it)
	}
	raw, _ := json.Marshal(items)
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatal("value in list output")
	}
}

// fakeSecretTool installs a secret-tool script as the only thing on PATH.
// It keeps values in files under its state dir and logs its argv.
func fakeSecretTool(t *testing.T, fail bool) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script fake")
	}
	bin, state := t.TempDir(), t.TempDir()
	script := `#!/bin/sh
PATH=/usr/bin:/bin
d='` + state + `'
printf '%s\n' "$*" >> "$d/argv.log"
if [ -f "$d/fail" ]; then
  echo "Cannot autolaunch D-Bus without X11 \$DISPLAY" >&2
  exit 1
fi
cmd=$1
eval "name=\${$#}"
case "$cmd" in
store) cat > "$d/s.$name" ;;
lookup) [ -f "$d/s.$name" ] || exit 1; cat "$d/s.$name" ;;
clear) [ -f "$d/s.$name" ] || exit 1; rm -f "$d/s.$name" ;;
search)
  found=0
  for f in "$d"/s.*; do
    [ -f "$f" ] || continue
    n=${f##*/s.}; found=1
    echo "[/org/freedesktop/secrets/collection/login/$n]"
    echo "label = Cerberus: $n"
    printf 'secret = '; cat "$f"; echo
    echo "modified = 2026-10-31 12:00:00"
    echo "schema = org.freedesktop.Secret.Generic"
    echo "attribute.app = cerberus" >&2
    echo "attribute.name = $n" >&2
  done
  [ "$found" = 1 ] || exit 1 ;;
*) echo "usage" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "secret-tool"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if fail {
		os.WriteFile(filepath.Join(state, "fail"), nil, 0o600)
	}
	t.Setenv("PATH", bin)
	return state
}

func TestSecretServiceViaFakeTool(t *testing.T) {
	state := fakeSecretTool(t, false)
	cfg, dir := testConfig(t, "secret-service")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if v.Backend() != BackendSecretService {
		t.Fatalf("backend %q", v.Backend())
	}
	const secret = "sk-live-0123456789"
	if err := v.Set("openai", secret, "note here"); err != nil {
		t.Fatal(err)
	}
	stored, err := os.ReadFile(filepath.Join(state, "s.openai"))
	if err != nil || string(stored) != secret {
		t.Fatalf("value did not arrive on stdin: %q %v", stored, err)
	}
	argv, _ := os.ReadFile(filepath.Join(state, "argv.log"))
	if bytes.Contains(argv, []byte(secret)) {
		t.Fatal("value appeared in argv")
	}
	if !bytes.Contains(argv, []byte("store --label=Cerberus: openai app cerberus name openai")) {
		t.Errorf("store argv:\n%s", argv)
	}
	checkMode(t, filepath.Join(dir, "vault-index.json"))
	idx, _ := os.ReadFile(filepath.Join(dir, "vault-index.json"))
	if bytes.Contains(idx, []byte(secret)) {
		t.Fatal("value in vault-index.json")
	}

	// A fresh vault reads it back through lookup.
	v2, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := v2.Get("openai"); err != nil || got != secret {
		t.Fatalf("lookup %q %v", got, err)
	}
	if _, err := v2.Get("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	items, err := v2.List()
	if err != nil || len(items) != 1 || items[0].Name != "openai" || items[0].Note != "note here" || items[0].Length != len(secret) {
		t.Fatalf("list %+v %v", items, err)
	}
	if raw, _ := json.Marshal(items); bytes.Contains(raw, []byte(secret)) {
		t.Fatal("value in list output")
	}
	if vals := v2.Values(); len(vals) != 1 || vals[0] != secret {
		t.Errorf("values %v", vals)
	}
	if err := v2.Delete("openai"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(state, "s.openai")); !os.IsNotExist(err) {
		t.Error("clear did not remove the item")
	}
	if err := v2.Delete("openai"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v", err)
	}
	if vals := v2.Values(); len(vals) != 1 || vals[0] != secret {
		t.Errorf("values after delete %v (the deleted value stays masked)", vals)
	}
}

func TestAutoFallsBackWhenSecretToolFails(t *testing.T) {
	fakeSecretTool(t, true)
	cfg, dir := testConfig(t, "auto")
	v, err := Open(cfg, quietLog())
	if err != nil {
		t.Fatal(err)
	}
	if v.Backend() != BackendFile {
		t.Fatalf("backend %q", v.Backend())
	}
	if err := v.Set("x", "value-x", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "vault.json")); err != nil {
		t.Fatal(err)
	}
	cfg.Vault.Backend = "secret-service"
	if _, err := Open(cfg, quietLog()); err == nil {
		t.Error("forced secret-service should fail when it does not answer")
	}
}

func TestAutoFallsBackWhenSecretToolAbsent(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cfg, _ := testConfig(t, "")
	v, err := Open(cfg, quietLog())
	if err != nil || v.Backend() != BackendFile {
		t.Fatalf("backend %v %v", v, err)
	}
}

func TestAutoPicksSecretService(t *testing.T) {
	fakeSecretTool(t, false)
	cfg, _ := testConfig(t, "auto")
	v, err := Open(cfg, quietLog())
	if err != nil || v.Backend() != BackendSecretService {
		t.Fatalf("backend %v %v", v, err)
	}
}

func TestGetIsMemoised(t *testing.T) {
	calls := map[string]int{}
	run := func(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, []byte, error) {
		calls[args[0]]++
		switch args[0] {
		case "lookup":
			return []byte("tok-" + args[len(args)-1]), nil, nil
		default:
			return nil, nil, nil
		}
	}
	cfg, _ := testConfig(t, "secret-service")
	v, err := Open(cfg, quietLog(), WithRunner(run))
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if got := v.Lookup("p"); got != "tok-p" {
			t.Fatalf("lookup %q", got)
		}
	}
	if calls["lookup"] != 1 {
		t.Errorf("lookups %d, want 1", calls["lookup"])
	}
}

func TestParseSearchDropsSecrets(t *testing.T) {
	out := []byte("[/p/1]\nlabel = Cerberus: a\nsecret = line1\nmodified = 2026-01-02 03:04:05\nattribute.app = cerberus\nattribute.name = a\nattribute.name = injected?\n[/p/2]\nsecret = s2\nattribute.name = b\n")
	got := dedupe(parseSearch(out))
	names := []string{}
	for _, f := range got {
		names = append(names, f.name)
	}
	// "injected?" fails name validation; a and b are kept; no value is retained.
	if strings.Join(names, ",") != "a,b" {
		t.Errorf("names %v", names)
	}
	if got[0].modified.IsZero() {
		t.Error("modified not parsed")
	}
}

// Auto keeps using the backend that already holds secrets.
func TestAutoIsSticky(t *testing.T) {
	state := fakeSecretTool(t, true) // keyring absent: the first start uses the file
	cfg, dir := testConfig(t, "auto")
	v, err := Open(cfg, quietLog())
	if err != nil || v.Backend() != BackendFile {
		t.Fatalf("first open %v %v", v, err)
	}
	if err := v.Set("k", "value-k", ""); err != nil {
		t.Fatal(err)
	}
	os.Remove(filepath.Join(state, "fail")) // the keyring answers on a later start
	v, err = Open(cfg, quietLog())
	if err != nil || v.Backend() != BackendFile {
		t.Fatalf("file vault with data should stay: %v %v", v, err)
	}
	if got := v.Lookup("k"); got != "value-k" {
		t.Fatalf("lookup %q", got)
	}

	// The other way round: secrets in the keyring, then a start where it
	// does not answer reports an error instead of an empty file vault.
	state = fakeSecretTool(t, false)
	cfg, dir = testConfig(t, "auto")
	v, err = Open(cfg, quietLog())
	if err != nil || v.Backend() != BackendSecretService {
		t.Fatalf("keyring open %v %v", v, err)
	}
	if err := v.Set("k", "value-k", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "fail"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(cfg, quietLog()); err == nil {
		t.Fatal("auto switched away from a keyring that holds secrets")
	}
	if exists(filepath.Join(dir, "vault.json")) {
		t.Error("an empty file vault was created")
	}
}
