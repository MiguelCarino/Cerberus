package api

// Settings export and import (Settings → Import & export), the web side of
// `cerberus export` / `cerberus import`. The bundle never holds a secret
// value (see config.Export).
//
//	GET  /api/config/export?sections=a,b&secret_names=1
//	     The profile bundle as a JSON download (cerberus-settings-<date>.json).
//	     sections defaults to every section, "models" included: the local
//	     models the engines list, each with the reference that downloads it
//	     again (an Ollama or LM Studio name, a Hugging Face .gguf link taken
//	     from the download's source note, or rebuilt from its
//	     <owner>/<repo>/<file> place), and its models[] entry when there is one.
//
//	POST /api/config/import {bundle: <the bundle>, apply: false|true}
//	     → ImportReply. apply=false only shows what would change; apply=true
//	     saves it (as PATCH /api/config does). A llama.cpp models[] entry
//	     whose path does not exist here is pointed at where its download
//	     lands. Files are never downloaded or deleted here: `models` lists
//	     each model of the bundle with present (an engine lists it now),
//	     registered (this config has a models[] entry for it) and ref, so
//	     the page can offer Download (POST /api/downloads) or Remove from
//	     the registry for each missing one. 400 not a bundle, 422 the
//	     bundle would make the config invalid, 409 config file problems.
//
//	POST /api/config/models/forget {ids: [...]}
//	     Removes those models[] entries (the registry only; no file is
//	     touched) → {removed: [...ids]}. Unknown ids are skipped.

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/engine"
)

// maxBundleBytes caps an uploaded bundle.
const maxBundleBytes = 4 << 20

// ImportChange is one setting an import changes. Values of sensitive
// settings are never sent.
type ImportChange struct {
	Path      string `json:"path"`
	Old       any    `json:"old,omitempty"`
	New       any    `json:"new,omitempty"`
	Restart   bool   `json:"restart,omitempty"`
	Sensitive bool   `json:"sensitive,omitempty"`
}

// ImportModel is one model of an imported bundle, as it stands here.
type ImportModel struct {
	ID         string `json:"id"`
	Backend    string `json:"backend"`
	Name       string `json:"name,omitempty"`
	Ref        string `json:"ref,omitempty"`
	SizeMiB    int    `json:"size_mib,omitempty"`
	Present    bool   `json:"present"`
	Registered bool   `json:"registered"`
}

// ImportReply answers POST /api/config/import.
type ImportReply struct {
	Applied     bool           `json:"applied"`
	Changes     []ImportChange `json:"changes"`
	Models      []ImportModel  `json:"models"`
	SecretNames []string       `json:"secret_names,omitempty"`
	Omitted     []string       `json:"omitted,omitempty"`
	Sections    []string       `json:"sections"`
	Created     time.Time      `json:"created,omitzero"`
	Changed     []ConfigChange `json:"changed,omitempty"`
}

func (s *server) bundleRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/config/export", s.handleConfigExport)
	mux.HandleFunc("POST /api/config/import", s.handleConfigImport)
	mux.HandleFunc("POST /api/config/models/forget", s.handleModelsForget)
}

// inventory is the local models for an export.
func (s *server) inventory(r *http.Request) []config.BundleModel {
	if s.models == nil {
		return nil
	}
	dirs := s.conf().Engines.LlamaCpp.ModelDirs
	var out []config.BundleModel
	for _, m := range s.models.Models(r.Context()) {
		if !m.Local {
			continue
		}
		out = append(out, config.BundleModel{ID: m.ID, Backend: m.Backend, Name: m.Name, Ref: modelRef(m, dirs), SizeMiB: m.SizeMiB})
	}
	return out
}

// modelRef is the reference that downloads m again, or "".
func modelRef(m engine.ModelInfo, dirs []string) string {
	switch m.Backend {
	case engine.BackendOllama, engine.BackendLMStudio:
		if m.Name != "" {
			return m.Name
		}
		return m.ID
	case engine.BackendLlamaCpp:
		if m.Path == "" {
			return ""
		}
		if ref := downloads.HFSource(m.Path); ref != "" {
			return ref
		}
		for _, d := range dirs {
			if rel, err := filepath.Rel(expandTilde(d), m.Path); err == nil && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel) {
				if ref := downloads.HFGuess(filepath.ToSlash(rel)); ref != "" {
					return ref
				}
			}
		}
	}
	return ""
}

func expandTilde(p string) string {
	if strings.HasPrefix(p, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (s *server) handleConfigExport(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can export settings")
		return
	}
	q := r.URL.Query()
	opts := config.ExportOptions{SecretNames: q.Get("secret_names") == "1" || q.Get("secret_names") == "true"}
	if v := strings.TrimSpace(q.Get("sections")); v != "" {
		opts.Sections = strings.Split(v, ",")
	}
	if len(opts.Sections) == 0 || slices.Contains(opts.Sections, config.SectionModels) {
		opts.Inventory = s.inventory(r)
	}
	raw, err := s.conf().Export(opts)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", `attachment; filename="cerberus-settings-`+time.Now().Format("2006-01-02")+`.json"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(append(raw, '\n'))
}

type importBody struct {
	Bundle json.RawMessage `json:"bundle"`
	Apply  bool            `json:"apply"`
}

// importPlace is where a bundle model's download lands here ("" unknown).
func (s *server) importPlace(c *config.Config) func(config.BundleModel) string {
	return func(m config.BundleModel) string {
		dirs := c.Engines.LlamaCpp.ModelDirs
		if m.Ref == "" || len(dirs) == 0 {
			return ""
		}
		p, err := downloads.HFDest(expandTilde(dirs[0]), m.Ref)
		if err != nil {
			return ""
		}
		return p
	}
}

func (s *server) handleConfigImport(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can import settings")
		return
	}
	var body importBody
	if err := decodeBodyLimit(w, r, &body, maxBundleBytes); err != nil {
		writeError(w, bodyErrStatus(err), "bad JSON: "+err.Error())
		return
	}
	b, err := config.ParseBundle(body.Bundle)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	cur := s.conf()
	changes, _, err := cur.ImportWith(body.Bundle, config.ImportOptions{Place: s.importPlace(cur)})
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, config.ErrInvalid) {
			status = http.StatusUnprocessableEntity
		}
		if body.Apply {
			noteAudit(r, "import refused: "+err.Error())
		}
		writeError(w, status, err.Error()+"; nothing was changed")
		return
	}
	reply := ImportReply{SecretNames: b.SecretNames, Omitted: b.Omitted, Sections: bundleSections(b), Created: b.Created, Changes: []ImportChange{}}
	paths := make([]string, 0, len(changes))
	set := map[string]any{}
	for _, ch := range changes {
		ic := ImportChange{Path: ch.Path, Restart: ch.Restart, Sensitive: ch.Sensitive}
		if !ch.Sensitive {
			ic.Old, ic.New = ch.Old, ch.New
			set[ch.Path] = ch.New
		}
		reply.Changes = append(reply.Changes, ic)
		paths = append(paths, ch.Path)
	}
	if body.Apply && len(set) > 0 {
		sort.Strings(paths)
		probs, changed, err := s.applyConfig(set)
		switch {
		case errors.Is(err, config.ErrInvalid):
			noteAudit(r, "import refused (invalid): "+strings.Join(paths, ", "))
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "the import makes the settings invalid; nothing was changed", "problems": probs})
			return
		case errors.Is(err, errConfigNoFile), errors.Is(err, errConfigDisk):
			noteAudit(r, "import refused: "+err.Error())
			writeError(w, http.StatusConflict, err.Error()+"; nothing was changed")
			return
		case err != nil:
			writeError(w, http.StatusInternalServerError, "save: "+err.Error())
			return
		}
		noteAudit(r, "imported "+changeNote(changed))
		reply.Applied, reply.Changed = true, changed
	} else if body.Apply {
		reply.Applied = true
	}
	reply.Models = s.importModels(r, b.Models)
	writeJSON(w, http.StatusOK, reply)
}

// importModels reports each bundle model against the engines and the live
// config.
func (s *server) importModels(r *http.Request, ms []config.BundleModel) []ImportModel {
	out := []ImportModel{}
	if len(ms) == 0 {
		return out
	}
	have := map[string]bool{}
	if s.models != nil {
		for _, m := range s.models.Models(r.Context()) {
			if m.Local {
				have[m.ID] = true
				have[m.Backend+"\x00"+m.Name] = true
			}
		}
	}
	reg := map[string]bool{}
	for _, mc := range s.conf().Models {
		reg[mc.ID] = true
	}
	for _, m := range ms {
		out = append(out, ImportModel{
			ID: m.ID, Backend: m.Backend, Name: m.Name, Ref: m.Ref, SizeMiB: m.SizeMiB,
			Present:    have[m.ID] || (m.Name != "" && have[m.Backend+"\x00"+m.Name]),
			Registered: reg[m.ID],
		})
	}
	return out
}

// bundleSections names the sections a bundle holds.
func bundleSections(b *config.Bundle) []string {
	has := map[string]bool{
		config.SectionAgents: b.Agents != nil || b.DefaultAgent != "", config.SectionRepos: b.Repos != nil || b.RepoRoots != nil,
		config.SectionProviders: b.Providers != nil, config.SectionPrices: b.Prices != nil, config.SectionBudgets: b.Budgets != nil,
		config.SectionTariff: b.Tariff != nil, config.SectionMonitor: b.Monitor != nil, config.SectionLimits: b.Limits != nil,
		config.SectionSandbox: b.Sandbox != nil, config.SectionPolicy: b.Policy != nil, config.SectionFiles: b.Files != nil,
		config.SectionModels: b.Models != nil,
	}
	out := []string{}
	for _, s := range config.BundleSections() {
		if has[s] {
			out = append(out, s)
		}
	}
	return out
}

type forgetBody struct {
	IDs []string `json:"ids"`
}

func (s *server) handleModelsForget(w http.ResponseWriter, r *http.Request) {
	if !principalOf(r).owner() {
		writeError(w, http.StatusForbidden, "only the owner can change settings")
		return
	}
	var body forgetBody
	if err := decodeBody(w, r, &body); err != nil {
		writeError(w, bodyErrStatus(err), "bad JSON: "+err.Error())
		return
	}
	drop := map[string]bool{}
	for _, id := range body.IDs {
		if id = strings.TrimSpace(id); id != "" {
			drop[id] = true
		}
	}
	if len(drop) == 0 {
		writeError(w, http.StatusBadRequest, `nothing to remove: send {"ids":["model id"]}`)
		return
	}
	cur := s.conf().Models
	removed := []string{}
	keep := make([]config.ModelConfig, 0, len(cur))
	for _, mc := range cur {
		if drop[mc.ID] {
			removed = append(removed, mc.ID)
			continue
		}
		keep = append(keep, mc)
	}
	if len(removed) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
		return
	}
	_, _, err := s.applyConfig(map[string]any{"models": keep})
	switch {
	case errors.Is(err, errConfigNoFile), errors.Is(err, errConfigDisk), errors.Is(err, config.ErrInvalid):
		noteAudit(r, "refused: "+err.Error())
		writeError(w, http.StatusConflict, err.Error()+"; nothing was changed")
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, "save: "+err.Error())
		return
	}
	noteAudit(r, "removed models[] entries: "+strings.Join(removed, ", "))
	writeJSON(w, http.StatusOK, map[string]any{"removed": removed})
}
