package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/agent"
)

// maxBatchRepos bounds one fleet task.
const maxBatchRepos = 200

// BatchSpec is one instruction for many repos: one repo session each.
type BatchSpec struct {
	Repos       []string // absolute repo paths (each must be a repo top level, validated like Create)
	Task        string   // may use {repo} (the repo's path) and {name} (its folder name)
	Agent       string
	Model       string
	EscalateTo  string
	MaxAttempts int
	Title       string
	Owner       string // user id (Phase 4a)
	Confined    bool   // the sessions run confined (a non-admin owner)
}

// BatchInfo describes a fleet task. Counts is sessions per state, live.
type BatchInfo struct {
	ID, Title, Task, Agent, Model string
	Owner                         string // user id (Phase 4a; "" = from before users)
	Created                       time.Time
	Sessions                      []string
	Counts                        map[string]int
}

// BatchResult is what AcceptBatch or DiscardBatch did with one session.
type BatchResult struct {
	SessionID, Repo string
	Result          string // accepted | skipped | failed | discarded
	Detail          string
	Commit          string
}

// Batch results.
const (
	ResultAccepted  = "accepted"
	ResultSkipped   = "skipped"
	ResultFailed    = "failed"
	ResultDiscarded = "discarded"
)

// RepoSelector picks repos for a fleet task. Empty fields match everything.
type RepoSelector struct {
	Roots    []string // folders whose direct git children are candidates; default RepoRoots (plus configured repos)
	HasFile  string   // a glob relative to the repo root that must match something, e.g. "carino-navbar.js"
	NameGlob string   // path.Match pattern on the repo's folder name (case-insensitive)
}

// RepoMatch is one repo FindRepos found. Dirty repos have uncommitted
// changes in the main checkout, so accepting into them may fail later.
type RepoMatch struct {
	Name, Path string
	Dirty      bool
}

// batch is the manager's record of a fleet task (DATA/batches/<id>.json).
// It is guarded by Manager.mu and never changes after creation.
type batch struct {
	ID         string    `json:"id"`
	Title      string    `json:"title"`
	Task       string    `json:"task"`
	Agent      string    `json:"agent"`
	Model      string    `json:"model"`
	EscalateTo string    `json:"escalate_to,omitempty"`
	Created    time.Time `json:"created"`
	Sessions   []string  `json:"sessions"`
	Repos      []string  `json:"repos"`
	Owner      string    `json:"owner,omitempty"`
}

// loadBatches reads DATA/batches. Unreadable files are skipped.
func (m *Manager) loadBatches() {
	entries, err := os.ReadDir(m.batchDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(m.batchDir, e.Name()))
		if err != nil {
			continue
		}
		var b batch
		if err := json.Unmarshal(raw, &b); err != nil || b.ID != id {
			m.log.Warn("skipping unreadable batch", "file", e.Name(), "err", err)
			continue
		}
		m.batches[b.ID] = &b
	}
}

func (m *Manager) saveBatch(b *batch) error {
	if err := os.MkdirAll(m.batchDir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return safeWrite(filepath.Join(m.batchDir, b.ID+".json"), raw, 0o600)
}

// CreateBatch validates every repo first (nothing is created if one fails),
// then queues one repo session per repo, all at once: the guard admits them
// as capacity allows. Each session's Group is the batch id.
func (m *Manager) CreateBatch(ctx context.Context, b BatchSpec) (BatchInfo, []Info, error) {
	task := strings.TrimSpace(b.Task)
	if task == "" {
		return BatchInfo{}, nil, errors.New("task is empty")
	}
	if len(b.Repos) == 0 {
		return BatchInfo{}, nil, errors.New("choose at least one repo")
	}
	if len(b.Repos) > maxBatchRepos {
		return BatchInfo{}, nil, fmt.Errorf("at most %d repos per fleet task", maxBatchRepos)
	}
	title := strings.TrimSpace(b.Title)
	if title == "" {
		title = titleFrom(task)
	}
	specs := make([]TaskSpec, 0, len(b.Repos))
	seen := map[string]bool{}
	for _, r := range b.Repos {
		t, err := m.normalize(ctx, TaskSpec{Workspace: WorkspaceRepo, Repo: r, Task: task, Agent: b.Agent,
			Model: b.Model, EscalateTo: b.EscalateTo, MaxAttempts: b.MaxAttempts, Title: title,
			Owner: b.Owner, Confined: b.Confined})
		if err != nil {
			return BatchInfo{}, nil, fmt.Errorf("%s: %w", r, err)
		}
		if seen[t.Repo] {
			continue
		}
		seen[t.Repo] = true
		name := filepath.Base(t.Repo)
		t.Task = strings.NewReplacer("{repo}", t.Repo, "{name}", name).Replace(task)
		specs = append(specs, t)
	}
	bt := &batch{ID: newID(), Title: title, Task: task, Agent: specs[0].Agent, Model: specs[0].Model,
		EscalateTo: specs[0].EscalateTo, Created: time.Now(), Owner: b.Owner}
	infos := make([]Info, 0, len(specs))
	for _, t := range specs {
		info, err := m.enqueue(m.ctx, t, title+" · "+filepath.Base(t.Repo), bt.ID, "", false)
		if err != nil {
			m.signal()
			return BatchInfo{}, infos, err
		}
		infos = append(infos, info)
		bt.Sessions = append(bt.Sessions, info.ID)
		bt.Repos = append(bt.Repos, t.Repo)
	}
	if err := m.saveBatch(bt); err != nil {
		m.log.Warn("save batch", "id", bt.ID, "err", err) // the sessions run regardless
	}
	m.mu.Lock()
	m.batches[bt.ID] = bt
	bi := m.batchInfoLocked(bt)
	m.mu.Unlock()
	m.signal()
	return bi, infos, nil
}

// batchInfoLocked is the public picture of b with live state counts.
func (m *Manager) batchInfoLocked(b *batch) BatchInfo {
	bi := BatchInfo{ID: b.ID, Title: b.Title, Task: b.Task, Agent: b.Agent, Model: b.Model, Created: b.Created, Owner: b.Owner,
		Sessions: slices.Clone(b.Sessions), Counts: map[string]int{}}
	for _, id := range b.Sessions {
		if s, ok := m.sessions[id]; ok {
			bi.Counts[s.info.State]++
		}
	}
	return bi
}

// Batches lists every fleet task, oldest first.
func (m *Manager) Batches() []BatchInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]BatchInfo, 0, len(m.batches))
	for _, b := range m.batches {
		out = append(out, m.batchInfoLocked(b))
	}
	slices.SortFunc(out, func(a, b BatchInfo) int {
		if c := a.Created.Compare(b.Created); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// Batch returns one fleet task and its sessions (pruned ones left out).
func (m *Manager) Batch(id string) (BatchInfo, []Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return BatchInfo{}, nil, false
	}
	infos := make([]Info, 0, len(b.Sessions))
	for _, sid := range b.Sessions {
		if s, ok := m.sessions[sid]; ok {
			infos = append(infos, m.snapshotLocked(s))
		}
	}
	return m.batchInfoLocked(b), infos, true
}

// batchMembers returns b's sessions in order with their repos.
func (m *Manager) batchMembers(id string) (*batch, []BatchResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.batches[id]
	if !ok {
		return nil, nil, ErrNotFound
	}
	out := make([]BatchResult, len(b.Sessions))
	for i, sid := range b.Sessions {
		out[i] = BatchResult{SessionID: sid}
		if i < len(b.Repos) {
			out[i].Repo = b.Repos[i]
		}
	}
	return b, out, nil
}

// AcceptBatch accepts every passed session of the batch, one after another
// (and never alongside another batch accept), so pushes to the same remote
// never race. Sessions in any other state are skipped with the reason.
// Without o.Message each commit is titled with the batch title.
func (m *Manager) AcceptBatch(ctx context.Context, id string, o AcceptOpts) []BatchResult {
	b, res, err := m.batchMembers(id)
	if err != nil {
		return []BatchResult{}
	}
	m.acceptMu.Lock()
	defer m.acceptMu.Unlock()
	if strings.TrimSpace(o.Message) == "" {
		o.Message = b.Title
	}
	for i := range res {
		r := &res[i]
		info, ok := m.Get(r.SessionID)
		switch {
		case !ok:
			r.Result, r.Detail = ResultSkipped, "session no longer exists"
			continue
		case info.State != StatePassed:
			r.Result, r.Detail = ResultSkipped, "session is "+info.State
			if info.Error != "" {
				r.Detail += ": " + firstLine(info.Error)
			}
			continue
		case ctx.Err() != nil:
			r.Result, r.Detail = ResultSkipped, "cancelled"
			continue
		}
		sha, err := m.Accept(ctx, r.SessionID, o)
		r.Commit = sha
		switch {
		case err == nil:
			r.Result = ResultAccepted
		case sha != "": // accepted locally; only the push failed
			r.Result, r.Detail = ResultAccepted, err.Error()
		default:
			r.Result, r.Detail = ResultFailed, err.Error()
		}
	}
	return res
}

// DiscardBatch discards every session of the batch that is not already
// accepted or discarded. Sessions are stopped in parallel, since each may
// wait for its agent to exit.
func (m *Manager) DiscardBatch(id string) []BatchResult {
	_, res, err := m.batchMembers(id)
	if err != nil {
		return []BatchResult{}
	}
	var wg sync.WaitGroup
	for i := range res {
		r := &res[i]
		info, ok := m.Get(r.SessionID)
		switch {
		case !ok:
			r.Result, r.Detail = ResultSkipped, "session no longer exists"
			continue
		case final(info.State):
			r.Result, r.Detail = ResultSkipped, "session is "+info.State
			continue
		}
		wg.Go(func() {
			if err := m.Discard(r.SessionID); err != nil {
				r.Result, r.Detail = ResultFailed, err.Error()
				return
			}
			r.Result = ResultDiscarded
		})
	}
	wg.Wait()
	return res
}

// pruneBatches deletes batches none of whose sessions remain.
func (m *Manager) pruneBatches() {
	m.mu.Lock()
	var gone []string
	for id, b := range m.batches {
		if !slices.ContainsFunc(b.Sessions, func(sid string) bool { _, ok := m.sessions[sid]; return ok }) {
			gone = append(gone, id)
			delete(m.batches, id)
		}
	}
	m.mu.Unlock()
	for _, id := range gone {
		if err := os.Remove(filepath.Join(m.batchDir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
			m.log.Warn("remove batch", "id", id, "err", err)
		}
	}
}

// FindRepos lists the git repos directly under sel.Roots (default RepoRoots
// and the configured repos) that match the selector, sorted by name. Repos
// protected by deny_paths and Cerberus' own folders are never offered.
func (m *Manager) FindRepos(sel RepoSelector) []RepoMatch {
	hasFile := strings.TrimSpace(sel.HasFile)
	if hasFile != "" {
		clean := filepath.Clean(filepath.FromSlash(hasFile))
		if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return []RepoMatch{} // only files inside the repo count
		}
		if _, err := filepath.Match(clean, ""); err != nil {
			return []RepoMatch{}
		}
		hasFile = clean
	}
	nameGlob := strings.ToLower(strings.TrimSpace(sel.NameGlob))
	if nameGlob != "" {
		if _, err := path.Match(nameGlob, ""); err != nil {
			return []RepoMatch{}
		}
	}

	var candidates []string
	roots := sel.Roots
	if len(roots) == 0 {
		roots = m.conf().RepoRoots
		for _, r := range m.conf().Repos {
			candidates = append(candidates, r.Path)
		}
	}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			candidates = append(candidates, filepath.Join(root, e.Name()))
		}
	}

	own := m.ownDirs()
	seen := map[string]bool{}
	out := []RepoMatch{}
	for _, c := range candidates {
		real, err := filepath.EvalSymlinks(c)
		if err != nil || seen[real] || !isGitDir(real) {
			continue
		}
		seen[real] = true
		if agent.DeniedBy(m.conf().Agent.DenyPaths, c) != "" || agent.DeniedBy(m.conf().Agent.DenyPaths, real) != "" {
			continue
		}
		if slices.ContainsFunc(own, func(o string) bool { return real == o || within(real, o) || within(o, real) }) {
			continue
		}
		name := filepath.Base(real)
		if nameGlob != "" {
			if ok, _ := path.Match(nameGlob, strings.ToLower(name)); !ok {
				continue
			}
		}
		if hasFile != "" {
			if matches, _ := filepath.Glob(filepath.Join(globEscape(real), hasFile)); len(matches) == 0 {
				continue
			}
		}
		st, err := runGit(m.ctx, real, nil, "status", "--porcelain")
		out = append(out, RepoMatch{Name: name, Path: real, Dirty: err != nil || strings.TrimSpace(st) != ""})
	}
	slices.SortFunc(out, func(a, b RepoMatch) int {
		if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
			return c
		}
		return strings.Compare(a.Path, b.Path)
	})
	return out
}

// globEscape quotes the glob metacharacters in a literal path, so a repo
// folder named "a[1]" still matches only itself.
func globEscape(p string) string {
	if runtime.GOOS == "windows" { // '\\' is the separator there, not an escape
		return p
	}
	var b strings.Builder
	for _, r := range p {
		if strings.ContainsRune(`*?[\`, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
