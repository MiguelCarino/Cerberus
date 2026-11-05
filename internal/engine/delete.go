package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Model deletion: a local model's files go from disk. Each engine deletes
// only inside its own models folder; cloud models cannot be deleted.

// ErrNotDeletable is returned for a model Cerberus cannot delete (a cloud
// model, or a llama.cpp model configured with a path outside model_dirs).
var ErrNotDeletable = errors.New("this model cannot be deleted from here")

// ErrInUse is returned while requests are running on the model.
var ErrInUse = errors.New("the model is answering requests; try again when it is idle")

// deleter is implemented by backends that can delete a downloaded model.
type deleter interface {
	Delete(ctx context.Context, m ModelInfo) error
}

// Delete removes a local model from disk: it is unloaded first (refused
// while requests run on it), then its engine deletes it, and the model list
// is read again.
func (m *Manager) Delete(ctx context.Context, id string) error {
	st, err := m.resolve(ctx, id)
	if err != nil {
		return err
	}
	st.loadMu.Lock()
	defer st.loadMu.Unlock()
	m.mu.Lock()
	info, b, busy := st.info, st.backend, st.inFlight
	m.mu.Unlock()
	if !info.Local {
		return fmt.Errorf("%w: %s is a cloud model", ErrNotDeletable, info.ID)
	}
	if busy > 0 {
		return ErrInUse
	}
	d, ok := b.(deleter)
	if !ok {
		return fmt.Errorf("%w: %s cannot delete models", ErrNotDeletable, b.Name())
	}
	if st.loaded {
		if err := b.Unload(ctx, info); err != nil {
			return fmt.Errorf("unload before delete: %w", err)
		}
		m.markUnloaded(st)
	}
	if err := d.Delete(ctx, info); err != nil {
		return err
	}
	m.log.Info("model deleted", "backend", b.Name(), "model", info.ID)
	m.ensureFresh(ctx, true)
	m.updateActive()
	return nil
}

// within reports whether p is root or below it.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// removeInside deletes target (a file, a symlink, or a folder when dirOK)
// after checking that it lies inside root both by its path and once
// symlinks in its folders are resolved, then removes folders it emptied up
// to root.
func removeInside(root, target string, dirOK bool) error {
	root, target = filepath.Clean(root), filepath.Clean(target)
	if root == "" || root == "." || target == root || !within(root, target) {
		return fmt.Errorf("%w: %s is outside %s", ErrNotDeletable, target, root)
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	realDir, err := filepath.EvalSymlinks(filepath.Dir(target))
	if err != nil {
		return err
	}
	if !within(realRoot, realDir) {
		return fmt.Errorf("%w: %s leads outside %s", ErrNotDeletable, target, root)
	}
	fi, err := os.Lstat(target)
	if err != nil {
		return err
	}
	switch {
	case fi.IsDir() && !dirOK:
		return fmt.Errorf("%w: %s is a folder", ErrNotDeletable, target)
	case fi.IsDir():
		err = os.RemoveAll(target)
	default:
		err = os.Remove(target) // a symlink goes, never what it points at
	}
	if err != nil {
		return err
	}
	for dir := filepath.Dir(target); dir != root && within(root, dir); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil { // only succeeds while empty
			break
		}
	}
	return nil
}

// ---- llama.cpp: the .gguf file, inside model_dirs

// SourceSuffix names the note a download leaves beside a .gguf file
// (<file>.gguf.source) holding the URL it came from, so an export can tell
// another machine how to download it again.
const SourceSuffix = ".source"

func (b *llamaCpp) Delete(ctx context.Context, m ModelInfo) error {
	b.mu.Lock()
	spec, ok := b.specs[m.ID]
	b.mu.Unlock()
	if !ok || spec.path == "" {
		return fmt.Errorf("%w: no file known for %s", ErrNotDeletable, m.ID)
	}
	for _, root := range b.cfg.ModelDirs {
		root = expandHome(root)
		if within(filepath.Clean(root), filepath.Clean(spec.path)) {
			// The download's source note (<file>.source) goes first, so the
			// empty folders above can be removed with the model.
			_ = removeInside(root, spec.path+SourceSuffix, false)
			if err := removeInside(root, spec.path, false); err != nil {
				return err
			}
			b.mu.Lock()
			delete(b.specs, m.ID)
			b.mu.Unlock()
			return nil
		}
	}
	return fmt.Errorf("%w: %s is not in a llamacpp.model_dirs folder (remove it and its models[] entry yourself)", ErrNotDeletable, spec.path)
}

// ---- LM Studio: the model's file or folder, inside its models folder

func (b *lmStudio) Delete(ctx context.Context, m ModelInfo) error {
	raw, err := b.lms(ctx, lmsCmdTimeout, "ls", "--json")
	if err != nil {
		return fmt.Errorf("lms ls: %w", err)
	}
	all, err := parseLMS(raw)
	if err != nil {
		return err
	}
	var rel string
	for _, e := range all {
		if e.key() == m.ID || e.ModelKey == m.Name {
			rel = e.Path
			break
		}
	}
	if rel == "" || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: LM Studio gave no path for %s", ErrNotDeletable, m.ID)
	}
	for _, root := range b.modelRoots() {
		target := filepath.Join(root, filepath.FromSlash(rel))
		if _, err := os.Lstat(target); err == nil {
			return removeInside(root, target, true)
		}
	}
	// A model from LM Studio's own catalogue ("qwen/qwen3.8-27b") is a
	// manifest in hub/models/<owner>/<name> naming the Hugging Face repos
	// that hold its files (lmstudio-community/Qwen3.8-27B-GGUF in a models
	// folder).
	if hub := b.hubRoot(); hub != "" {
		if dir := filepath.Join(hub, filepath.FromSlash(rel)); fileExists(filepath.Join(dir, "manifest.json")) {
			return b.deleteHub(hub, dir)
		}
	}
	return fmt.Errorf("%w: the files of %s were not found in LM Studio's models folder", ErrNotDeletable, m.ID)
}

// hubRoot is LM Studio's catalogue folder (~/.lmstudio/hub/models).
func (b *lmStudio) hubRoot() string {
	if b.hub != nil {
		return b.hub()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".lmstudio", "hub", "models")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// lmsSegment is one plain path segment of a Hugging Face user or repo.
var lmsSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// deleteHub removes the files of a catalogue model (each source repo
// folder found in a models folder, matched without case) and then its
// manifest folder.
func (b *lmStudio) deleteHub(hub, dir string) error {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return err
	}
	var man struct {
		Dependencies []struct {
			Sources []struct {
				Type string `json:"type"`
				User string `json:"user"`
				Repo string `json:"repo"`
			} `json:"sources"`
		} `json:"dependencies"`
	}
	if err := json.Unmarshal(raw, &man); err != nil {
		return fmt.Errorf("%w: LM Studio's manifest %s: %v", ErrNotDeletable, dir, err)
	}
	for _, d := range man.Dependencies {
		for _, src := range d.Sources {
			if src.Type != "huggingface" || !lmsSegment.MatchString(src.User) || !lmsSegment.MatchString(src.Repo) || strings.Contains(src.User+src.Repo, "..") {
				continue
			}
			for _, root := range b.modelRoots() {
				user := matchEntry(root, src.User)
				if user == "" {
					continue
				}
				repo := matchEntry(filepath.Join(root, user), src.Repo)
				if repo == "" {
					continue
				}
				if err := removeInside(root, filepath.Join(root, user, repo), true); err != nil {
					return err
				}
			}
		}
	}
	return removeInside(hub, dir, true)
}

// matchEntry finds the folder in dir named name, ignoring case ("" none).
func matchEntry(dir, name string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range ents {
		if e.IsDir() && strings.EqualFold(e.Name(), name) {
			return e.Name()
		}
	}
	return ""
}

// modelRoots are the folders LM Studio may keep models in: its settings'
// downloadsFolder, then the current and the old default.
func (b *lmStudio) modelRoots() []string {
	if b.roots != nil {
		return b.roots()
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	var out []string
	if raw, err := os.ReadFile(filepath.Join(home, ".lmstudio", "settings.json")); err == nil {
		var s struct {
			DownloadsFolder string `json:"downloadsFolder"`
		}
		if json.Unmarshal(raw, &s) == nil && s.DownloadsFolder != "" {
			out = append(out, expandHome(s.DownloadsFolder))
		}
	}
	return append(out, filepath.Join(home, ".lmstudio", "models"), filepath.Join(home, ".cache", "lm-studio", "models"))
}

// ---- Ollama: its own delete API

func (b *ollama) Delete(ctx context.Context, m ModelInfo) error {
	raw, _ := json.Marshal(map[string]string{"model": modelName(m), "name": modelName(m)})
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, b.url+"/api/delete", bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		b.markDown()
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ollama delete: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	return nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
