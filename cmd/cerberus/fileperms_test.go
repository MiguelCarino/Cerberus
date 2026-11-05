//go:build !windows

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"carino.systems/cerberus/internal/config"
)

// TestServeFileModes (day phase 7, cross-cutting checks) runs the daemon
// on a temp HOME/XDG through its main write paths (config save, the file
// vault, paired devices, the audit log, the inbox, chats, session meta,
// output, feed and recordings, staged uploads) and then checks that every
// file under DATA and the config dir is 0600 and every folder 0700.
//
// A session's workspace (scratch folder or worktree) is the agent's
// working copy, not Cerberus' own state: its contents keep ordinary modes
// so builds and tools work, and they sit inside the session's 0700 folder.
// Model files (downloads, in the configured model folders) are checked in
// internal/downloads.
func TestServeFileModes(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	dataHome, cfgHome := filepath.Join(tmp, "data"), filepath.Join(tmp, "config")
	for _, d := range []string{home, dataHome, cfgHome} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_DATA_HOME", dataHome)
	t.Setenv("XDG_CONFIG_HOME", cfgHome)
	// With umask 0 a mode Cerberus asks for is the mode it gets: nothing
	// can rely on the user's umask to narrow it.
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })
	agent := filepath.Join(tmp, "agent.sh")
	if err := os.WriteFile(agent, []byte("#!/bin/bash\necho \"working on: $1\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := config.Defaults()
	cfg.Server.Listen = "127.0.0.1:0"
	cfg.Server.Token = "tok"
	cfg.GPU.Backend = "none"
	cfg.Engines.LlamaCpp.Enabled = false
	cfg.Engines.LMStudio.Enabled = false
	cfg.Engines.Ollama.Enabled = false
	cfg.Notify.Disabled = true
	cfg.RepoRoots, cfg.Repos = nil, nil
	cfg.Vault.Backend = "file"
	cfg.Subscriptions = nil
	cfg.Voice.Enabled = false
	cfg.Agents = []config.AgentProfile{{Name: "fake", Command: []string{"bash", agent, "{task}"}, Sandbox: "none"}}
	cfg.DefaultAgent = "fake"
	cfgDir := filepath.Join(cfgHome, "cerberus")
	cfgPath := filepath.Join(cfgDir, "config.json")
	cfg.SetPath(cfgPath)
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan []string, 1)
	done := make(chan error, 1)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	go func() { done <- serve(ctx, func() {}, cfgPath, log, func(a []string) { ready <- a }) }()
	var base string
	select {
	case addrs := <-ready:
		base = "http://" + addrs[0]
	case err := <-done:
		t.Fatalf("serve exited: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve not ready")
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("serve: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Error("serve did not shut down")
		}
	}
	defer stop()

	call := func(method, path, ctype string, body io.Reader, out any) int {
		t.Helper()
		req, _ := http.NewRequest(method, base+path, body)
		req.Header.Set("Authorization", "Bearer tok")
		if ctype != "" {
			req.Header.Set("Content-Type", ctype)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode >= 300 {
			t.Errorf("%s %s: %d %s", method, path, resp.StatusCode, raw)
		} else if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				t.Fatalf("%s %s: %v (%s)", method, path, err, raw)
			}
		}
		return resp.StatusCode
	}
	jsonCall := func(method, path, body string, out any) int {
		t.Helper()
		return call(method, path, "application/json", strings.NewReader(body), out)
	}
	upload := func(path string, files map[string]string, out any) {
		t.Helper()
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		for name, content := range files {
			fw, _ := mw.CreateFormFile("file", name)
			_, _ = io.WriteString(fw, content)
		}
		_ = mw.Close()
		call("POST", path, mw.FormDataContentType(), &buf, out)
	}

	jsonCall("PATCH", "/api/config", `{"set":{"sessions.max_attempts":2}}`, nil)
	jsonCall("PUT", "/api/secrets/OPENAI", `{"value":"sk-test-123456"}`, nil)
	jsonCall("POST", "/api/devices", `{"name":"phone"}`, nil)
	jsonCall("POST", "/api/chats", `{}`, nil)

	// Two staged uploads: one stays staged, one goes with a task.
	var kept, used struct {
		Staging string `json:"staging"`
	}
	upload("/api/uploads", map[string]string{"notes.txt": "kept", ".env": "SECRET=1"}, &kept)
	upload("/api/uploads", map[string]string{"brief.md": "attached", ".npmrc": "//registry:_authToken=x"}, &used)
	if kept.Staging == "" || used.Staging == "" {
		t.Fatalf("staging ids %q %q", kept.Staging, used.Staging)
	}
	var created []struct {
		ID string `json:"id"`
	}
	jsonCall("POST", "/api/sessions", `{"Task":"count files","Agent":"fake","Workspace":"scratch","Staging":"`+used.Staging+`"}`, &created)
	if len(created) != 1 {
		t.Fatalf("created %v", created)
	}
	id := created[0].ID
	deadline := time.Now().Add(20 * time.Second)
	for {
		var info struct {
			State string `json:"state"`
		}
		jsonCall("GET", "/api/sessions/"+id, "", &info)
		if info.State == "passed" || info.State == "failed" || info.State == "error" || info.State == "stopped" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session still %q", info.State)
		}
		time.Sleep(100 * time.Millisecond)
	}
	jsonCall("PATCH", "/api/sessions/"+id, `{"name":"renamed","tags":["perm"]}`, nil)
	upload("/api/sessions/"+id+"/files", map[string]string{"later.txt": "dropped"}, nil)
	jsonCall("POST", "/api/inbox/read", `{"all":true}`, nil)
	jsonCall("POST", "/api/token/rotate", `{}`, nil)
	stop()

	dataDir := filepath.Join(dataHome, "cerberus")
	seen := map[string]bool{}
	for _, root := range []string{dataDir, cfgDir} {
		err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(dataDir, p)
			if workspace(rel) {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			st, err := os.Lstat(p)
			if err != nil {
				return err
			}
			seen[filepath.Base(p)] = true
			mode := st.Mode()
			switch {
			case mode&fs.ModeSymlink != 0:
				t.Errorf("%s: unexpected symlink", p)
			case mode.IsDir() && mode.Perm() != 0o700:
				t.Errorf("%s: dir mode %o, want 700", p, mode.Perm())
			case mode.IsRegular() && mode.Perm() != 0o600:
				t.Errorf("%s: file mode %o, want 600", p, mode.Perm())
			case !mode.IsDir() && !mode.IsRegular():
				t.Errorf("%s: unexpected file type %v", p, mode)
			}
			if strings.Contains(filepath.Base(p), ".tmp") {
				t.Errorf("%s: a temp file was left behind (non-atomic write?)", p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// The workspace got ordinary modes back, and the secret-ish upload
	// stayed private on its way through staging (where both were 0600).
	inbox := filepath.Join(dataDir, "sessions", id, "workspace", "inbox")
	for name, want := range map[string]fs.FileMode{"brief.md": 0o644, "npmrc": 0o600} {
		if st, err := os.Stat(filepath.Join(inbox, name)); err != nil || st.Mode().Perm() != want {
			t.Errorf("workspace inbox %s: %v %v, want %o", name, st, err, want)
		}
	}
	for _, want := range []string{"config.json", "devices.json", "audit.jsonl", "inbox.jsonl", "usage.jsonl"} {
		if !seen[want] {
			t.Errorf("%s was not written (write path not exercised?)", want)
		}
	}
	if t.Failed() {
		_ = filepath.WalkDir(tmp, func(p string, d fs.DirEntry, err error) error {
			if err == nil {
				if st, e := os.Lstat(p); e == nil {
					t.Logf("%v %s", st.Mode(), strings.TrimPrefix(p, tmp))
				}
			}
			return nil
		})
	}
}

// workspace reports whether rel (relative to DATA) is a session's
// workspace or inside one: DATA/sessions/<id>/workspace (scratch) or
// DATA/worktrees/<id> (a repo session's git worktree). Their parents are
// 0700 and checked.
func workspace(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	switch {
	case len(parts) >= 3 && parts[0] == "sessions" && parts[2] == "workspace":
		return true
	case len(parts) >= 2 && parts[0] == "worktrees":
		return true
	}
	return false
}
