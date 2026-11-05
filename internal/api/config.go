package api

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
)

// Configuration endpoints (day phase 4):
//
//	GET   /api/config                     {config (no token), problems, restart_pending, pending, live}
//	GET   /api/config/schema              {groups, fields}: every setting with its words and limits
//	PATCH /api/config {"set":{path:value}} atomic: all changes or none; 422 with problems when invalid
//	POST  /api/config/restart             clean shutdown, then the daemon starts again
//	                                      (409 when the saved config would not start)
//	POST  /api/config/init-done           first-run welcome finished (server.init_done = true)
//
// A successful PATCH is saved to config.json at once, then handed to every
// Reloader (engine, gateway, notifier, monitor, energy, alerts, sessions) so
// settings that need no restart apply live. Changes to the rest mark
// restart_pending until the daemon restarts. Writes are owner-only and
// audited (the middleware records them; the note names the paths changed).
//
// The live config is an immutable snapshot: each change builds a fresh clone,
// saves it and swaps it in (s.live), so readers never race a writer. The
// startup config (s.cfg) keeps the admin token, which rotation updates.

// Reloader is a component that applies a new config without a restart. The
// config it gets is never changed afterwards.
type Reloader interface {
	Reload(cfg *config.Config)
}

// livePrefixes are the settings the Reload hooks apply without a restart
// (schema paths, or prefixes ending in "."). Anything else that the schema
// does not flag as needing a restart is still reported as restart_pending,
// because the component reading it has no hook yet.
var livePrefixes = []string{
	"engines.default_model", "engines.idle_ttl_sec", "engines.allow_overcommit", // engine
	"prices", "budgets", // gateway
	"agents", "default_agent", "agent.approve", "agent.max_steps", "agent.command_timeout_s",
	"agent.spawn.", "agent.policy.", "context.", "repos", "repo_roots", "sessions.", "guard.",
	"git.", "sandbox.", // sessions
	"files.", "agent.deny_paths", // sessions and the files store
	"notify.",                    // notifier
	"monitor.alerts.", "tariff.", // alerts and energy
	"monitor.protected",                                             // monitor
	"monitor.cpu_idle_w", "monitor.cpu_max_w", "monitor.baseline_w", // energy
	"monitor.psu_efficiency", "monitor.extra_w",
	"server.public_url", "server.init_done", // the API itself
	"chat.",  // the LLM chat (reads the live config on every use)
	"flows.", // the flow engine (FlowReloader)
}

// isLive reports whether a change at schema path p applies without restart.
func isLive(p string) bool {
	for _, pre := range livePrefixes {
		if p == pre || (strings.HasSuffix(pre, ".") && strings.HasPrefix(p, pre)) {
			return true
		}
	}
	return false
}

// ConfigView is the reply of GET /api/config and PATCH /api/config.
type ConfigView struct {
	Config         *config.Config   `json:"config"` // sensitive settings emptied
	Problems       []config.Problem `json:"problems"`
	RestartPending bool             `json:"restart_pending"`
	Pending        []string         `json:"pending"` // settings changed since start that wait for a restart
	Restartable    bool             `json:"restartable"`
	Changed        []ConfigChange   `json:"changed,omitempty"` // PATCH only: what this request changed
}

// ConfigChange is one setting a PATCH changed.
type ConfigChange struct {
	Path    string `json:"path"`
	Live    bool   `json:"live"` // applied now; false: waits for a restart
	Restart bool   `json:"restart"`
}

// ConfigPatch is the body of PATCH /api/config.
type ConfigPatch struct {
	Set map[string]any `json:"set"`
}

// ConfigSchema is the reply of GET /api/config/schema.
type ConfigSchema struct {
	Groups []string       `json:"groups"`
	Fields []config.Field `json:"fields"`
}

func (s *server) configRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/config", s.handleConfigGet)
	mux.HandleFunc("GET /api/config/schema", s.handleConfigSchema)
	mux.HandleFunc("PATCH /api/config", s.handleConfigPatch)
	mux.HandleFunc("POST /api/config/restart", s.handleConfigRestart)
	mux.HandleFunc("POST /api/config/init-done", s.handleConfigInitDone)
}

// conf is the live config: the last applied change, else the startup one.
// Callers must not change it.
func (s *server) conf() *config.Config {
	if c := s.live.Load(); c != nil {
		return c
	}
	return s.cfg
}

// view is the live config with the current admin token (a private copy),
// for code that reads both, such as the doctor.
func (s *server) view() *config.Config {
	if s.live.Load() == nil {
		return s.cfg
	}
	n := s.conf().Clone()
	n.Server.Token, n.Server.TokenCreated = s.token(), s.cfg.TokenCreatedAt()
	return n
}

// afterRotate keeps the live snapshot's token in step with a rotation.
// Caller holds s.cfgMu (lock order: cfgMu, then tokMu).
func (s *server) afterRotate() {
	if cur := s.live.Load(); cur != nil {
		n := cur.Clone()
		n.Server.Token, n.Server.TokenCreated = s.cfg.TokenSnapshot(), s.cfg.TokenCreatedAt()
		s.live.Store(n)
	}
}

// validateOpts checks secret references against the vault, when there is one.
func (s *server) validateOpts() config.ValidateOptions {
	if s.secrets == nil {
		return config.ValidateOptions{}
	}
	items, err := s.secrets.List()
	if err != nil {
		return config.ValidateOptions{}
	}
	names := map[string]bool{}
	for _, it := range items {
		names[it.Name] = true
	}
	return config.ValidateOptions{SecretExists: func(n string) bool { return names[n] }}
}

// pending lists the settings changed since start that wait for a restart.
func (s *server) pending() []string {
	var out []string
	for _, ch := range config.Diff(s.boot, s.conf()) {
		if ch.Sensitive || ch.Path == "server.token_created" {
			continue // rotation applies at once
		}
		if ch.Restart || !isLive(ch.Path) {
			out = append(out, ch.Path)
		}
	}
	return out
}

func (s *server) configView(c *config.Config) ConfigView {
	p := s.pending()
	probs := c.ValidateWith(s.validateOpts())
	if probs == nil {
		probs = []config.Problem{}
	}
	if p == nil {
		p = []string{}
	}
	return ConfigView{Config: c.Redacted(), Problems: probs, RestartPending: len(p) > 0, Pending: p,
		Restartable: s.restart != nil}
}

func (s *server) handleConfigGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.configView(s.conf()))
}

func (s *server) handleConfigSchema(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, ConfigSchema{Groups: config.Groups(), Fields: config.Schema()})
}

// errConfigNoFile: a config without a file cannot take changes (they would
// otherwise be lost, or written to the default path).
var errConfigNoFile = errors.New("the config has no file to save changes to")

// errConfigDisk: config.json changed on disk since the daemon last read or
// wrote it and cannot be read now, so a change could not keep that edit.
var errConfigDisk = errors.New("config.json was changed outside Cerberus and cannot be read")

// noteDisk records config.json as it is now as the daemon's own copy.
// Caller holds s.cfgMu, or is newServer.
func (s *server) noteDisk() {
	if p := s.cfg.Path(); p != "" {
		if raw, err := os.ReadFile(p); err == nil {
			s.diskSum = sha256.Sum256(raw)
		}
	}
}

// applyConfig sets every change on a fresh clone of the live config,
// validates it, saves it and swaps it in, then runs the Reload hooks. On a
// validation failure it returns the problems and ErrInvalid; nothing changes.
//
// When config.json changed on disk since the daemon last read or wrote it
// (a hand edit, or the CLI writing the file because it could not reach this
// daemon), the change is made on the file's contents instead, so that edit
// is kept, and it is applied live (or marked pending) with the rest. A file
// that no longer parses refuses the change (errConfigDisk) rather than
// overwriting the edit.
func (s *server) applyConfig(set map[string]any) ([]config.Problem, []ConfigChange, error) {
	if s.cfg.Path() == "" {
		return nil, nil, errConfigNoFile
	}
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	cur := s.conf()
	base := cur
	if raw, err := os.ReadFile(s.cfg.Path()); err == nil && sha256.Sum256(raw) != s.diskSum {
		d, perr := config.Parse(s.cfg.Path(), raw)
		if perr != nil {
			return nil, nil, fmt.Errorf("%w: %v; fix it (cerberus config validate) and try again", errConfigDisk, perr)
		}
		s.log.Info("config.json changed on disk; keeping that edit in this change")
		base = d
	}
	n := base.Clone()
	probs, err := n.SetAllWith(set, s.validateOpts())
	if err != nil {
		return probs, nil, err
	}
	// The clone may predate a rotation: the file must keep today's token.
	n.Server.Token, n.Server.TokenCreated = s.token(), s.cfg.TokenCreatedAt()
	n.SetPath(s.cfg.Path())
	if err := n.Save(); err != nil {
		return nil, nil, err
	}
	s.noteDisk()
	var changed []ConfigChange
	for _, ch := range config.Diff(cur, n) {
		if ch.Sensitive {
			continue
		}
		changed = append(changed, ConfigChange{Path: ch.Path, Restart: ch.Restart, Live: !ch.Restart && isLive(ch.Path)})
	}
	s.live.Store(n)
	if len(changed) > 0 {
		for _, rl := range s.reloaders {
			rl.Reload(n)
		}
	}
	return probs, changed, nil
}

func (s *server) handleConfigPatch(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can change settings")
		return
	}
	var body ConfigPatch
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, bodyErrStatus(err), "bad JSON: "+err.Error())
		return
	}
	if len(body.Set) == 0 {
		writeError(w, http.StatusBadRequest, `nothing to change: send {"set":{"path":value}}`)
		return
	}
	paths := make([]string, 0, len(body.Set))
	for p := range body.Set {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	probs, changed, err := s.applyConfig(body.Set)
	switch {
	case errors.Is(err, config.ErrInvalid):
		noteAudit(r, "refused (invalid): "+strings.Join(paths, ", "))
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid settings; nothing was changed",
			"problems": probs})
		return
	case errors.Is(err, errConfigNoFile), errors.Is(err, errConfigDisk):
		noteAudit(r, "refused: "+err.Error())
		writeError(w, http.StatusConflict, err.Error()+"; nothing was changed")
		return
	case err != nil && (errors.Is(err, config.ErrNoPath) || errors.Is(err, config.ErrType) ||
		errors.Is(err, config.ErrSensitive) || errors.Is(err, config.ErrReadOnly)):
		noteAudit(r, "refused: "+err.Error())
		path, msg, _ := strings.Cut(err.Error(), ": ")
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error() + "; nothing was changed",
			"problems": []config.Problem{{Path: path, Message: msg, Level: config.LevelError}}})
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "save: "+err.Error())
		return
	}
	noteAudit(r, "changed "+changeNote(changed))
	v := s.configView(s.conf())
	if len(probs) > 0 {
		v.Problems = probs
	}
	v.Changed = changed
	writeJSON(w, http.StatusOK, v)
}

// changeNote names the changed settings for the audit record (never values:
// a setting may hold a URL with a key in it).
func changeNote(changed []ConfigChange) string {
	if len(changed) == 0 {
		return "nothing (same values)"
	}
	parts := make([]string, 0, len(changed))
	for _, c := range changed {
		tag := "live"
		if !c.Live {
			tag = "restart"
		}
		parts = append(parts, c.Path+" ("+tag+")")
	}
	return strings.Join(parts, ", ")
}

func (s *server) handleConfigInitDone(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can finish setup")
		return
	}
	if s.conf().Server.InitDone {
		noteAudit(r, "setup already finished")
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "init_done": true})
		return
	}
	if _, _, err := s.applyConfig(map[string]any{"server.init_done": true}); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, errConfigNoFile) || errors.Is(err, errConfigDisk) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	noteAudit(r, "first-run setup finished")
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "init_done": true})
}

// restartDelay lets the reply reach the client before shutdown begins.
var restartDelay = 200 * time.Millisecond

func (s *server) handleConfigRestart(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can restart Cerberus")
		return
	}
	if s.restart == nil {
		unavailable(w, "restart")
		return
	}
	if s.restartOK != nil {
		if err := s.restartOK(); err != nil {
			noteAudit(r, "restart refused: "+err.Error())
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":    "Cerberus would not start again with the saved settings, so it keeps running: " + err.Error(),
				"problems": []config.Problem{{Path: "server.listen", Message: err.Error(), Level: config.LevelError}}})
			return
		}
	}
	pend := s.pending()
	noteAudit(r, "restart requested; pending: "+orDefault(strings.Join(pend, ", "), "none"))
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "restarting": true, "pending": slices.Clip(pend)})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	restart := s.restart
	time.AfterFunc(restartDelay, restart)
}
