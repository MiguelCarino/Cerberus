package api

// Fine-tuning: review the training examples, rate and exclude them, and run
// the owner's trainer as a session. Every route is owner scope (never an
// agent token); writes send the X-Cerberus header like any other.
//
// JSON contract (for the web UI):
//
//	GET  /api/finetune
//	  → {capture: "off"|"delegated"|"cloud", stats?: Stats}
//	    Stats {dir, examples, by_kind: {delegated, cloud}, rated, good,
//	           corrected, excluded, usable, usable_unrated, bytes, oldest?, newest?}
//	    stats is absent when there is no example store.
//
//	GET  /api/finetune/export?unrated=1&kind=delegated|cloud
//	  → chat JSONL, one {"messages": [...]} per line (a download;
//	    X-Cerberus-Rows says how many). Excluded examples are never in it.
//
//	GET  /api/finetune/examples?kind=&rated=good|bad|corrected|unrated&q=&limit=&before=
//	  → {examples: [Example], total, next}
//	    newest first; limit 1..200 (default 20); before is the previous
//	    page's next (an example id), next is "" on the last page; total
//	    counts every match. Messages, answer and rating.better are clipped
//	    to 4 KiB each here, with truncated: true when any was.
//	    400 for a bad kind, rated, limit or before.
//	  Example {id, time, kind: "delegated"|"cloud", model, session?,
//	           messages: [{role, content}], answer,
//	           rating: null | {id, time, good, better?, note?, by?: "owner"|"agent"},
//	           excluded: bool, truncated?: bool}
//	    rating is the newest verdict, whoever gave it (by absent: an
//	    agent, on older records). Rating state for filters: better set =
//	    corrected, else good or bad; null = unrated.
//
//	GET  /api/finetune/examples/{id}
//	  → Example, whole (never truncated). 404 unknown id.
//
//	POST /api/finetune/examples/{id}/rate   {good: bool, better?: string, note?: string}
//	  → Example (whole) with the new rating (by: "owner"). It replaces any
//	    agent's verdict; a later verdict replaces it. 404 unknown id, 413 too
//	    big. better is the answer it should have been: export uses it in
//	    place of the model's answer.
//
//	POST /api/finetune/examples/{id}/exclude   {exclude: bool}
//	  → Example (whole). Excluded examples stay on disk and keep their
//	    rating, but no export or training run uses them. 400 without
//	    exclude, 404 unknown id.
//
//	POST /api/finetune/train   {base?: string, unrated?: bool}
//	  → 201 session Info (as GET /api/sessions/{id}), header X-Cerberus-Run:
//	    the run id. Exports the usable rows (with unrated: also delegated
//	    answers nobody rated) into DATA/finetune/runs/<run>/dataset.jsonl
//	    and starts finetune.train_command as a scratch session titled
//	    "Fine-tune <base>" under the internal agent "finetune-train" (never
//	    offered in pickers). {dataset} is the copy in the session's
//	    workspace, {out} the run folder, {base_model} base (default
//	    finetune.base_model). Open the session like any other (#/s/<ID>).
//	    409 {error}: no trainer set (finetune.train_command), the trainer
//	    is not installed, or no usable rows. 400 bad base. 503 without an
//	    example store or sessions.
//
//	GET  /api/finetune/runs
//	  → {runs: [Run]} newest first.
//	  Run {id, time, rows, base, unrated?, session?, dir, dataset, out,
//	       command?, state}
//	    state is the session's state (queued, running, passed, failed,
//	    stopped…), "removed" when its session was deleted, or "cli" for a
//	    run started with `cerberus finetune train`. session is the session
//	    id to link to (#/s/<session>).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/finetune"
	"carino.systems/cerberus/internal/session"
)

// trainService starts fine-tuning runs (a *session.Manager).
type trainService interface {
	StartTrain(ctx context.Context, t session.TrainSpec) (session.Info, error)
}

func (s *server) finetuneRoutes(mux *routeMux) {
	mux.HandleFunc("GET /api/finetune", s.handleFinetuneStats)
	mux.HandleFunc("GET /api/finetune/export", s.handleFinetuneExport)
	mux.HandleFunc("GET /api/finetune/examples", s.handleFinetuneList)
	mux.HandleFunc("GET /api/finetune/examples/{id}", s.handleFinetuneGet)
	mux.HandleFunc("POST /api/finetune/examples/{id}/rate", s.handleFinetuneRate)
	mux.HandleFunc("POST /api/finetune/examples/{id}/exclude", s.handleFinetuneExclude)
	mux.HandleFunc("POST /api/finetune/train", s.handleFinetuneTrain)
	mux.HandleFunc("GET /api/finetune/runs", s.handleFinetuneRuns)
}

func (s *server) handleFinetuneStats(w http.ResponseWriter, r *http.Request) {
	capture := s.conf().Finetune.Capture
	if capture == "" {
		capture = config.CaptureOff
	}
	out := map[string]any{"capture": capture}
	if s.finetune != nil {
		st, err := s.finetune.Stats()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		out["stats"] = st
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) handleFinetuneExport(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	q := r.URL.Query()
	o := finetune.ExportOptions{Unrated: q.Get("unrated") == "1"}
	if k := q.Get("kind"); k != "" {
		if k != finetune.KindDelegated && k != finetune.KindCloud {
			writeError(w, http.StatusBadRequest, "kind must be delegated or cloud")
			return
		}
		o.Kinds = []string{k}
	}
	var buf bytes.Buffer
	n, err := s.finetune.Export(&buf, o)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/jsonl")
	w.Header().Set("Content-Disposition", `attachment; filename="cerberus-finetune-`+time.Now().Format("20060102")+`.jsonl"`)
	w.Header().Set("X-Cerberus-Rows", strconv.Itoa(n))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

func (s *server) handleFinetuneList(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	q := r.URL.Query()
	o := finetune.ListOptions{Kind: q.Get("kind"), Rated: q.Get("rated"), Q: q.Get("q"), Before: q.Get("before")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			writeError(w, http.StatusBadRequest, "limit must be 1 to 200")
			return
		}
		o.Limit = n
	}
	page, err := s.finetune.List(o)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, finetune.ErrBadCursor) || !finetune.ValidKind(o.Kind) || !finetune.ValidRated(o.Rated) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, page)
}

// finetuneErr answers a store error: 404 for an unknown example, 413 for
// one too big, 500 otherwise.
func finetuneErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, finetune.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, finetune.ErrTooBig):
		writeError(w, http.StatusRequestEntityTooLarge, "too large (finetune.max_row_kib)")
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (s *server) handleFinetuneGet(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	it, err := s.finetune.Get(r.PathValue("id"))
	if err != nil {
		finetuneErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

func (s *server) handleFinetuneRate(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	id := r.PathValue("id")
	var b struct {
		Good   bool   `json:"good"`
		Better string `json:"better"`
		Note   string `json:"note"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	verdict := "bad"
	switch {
	case strings.TrimSpace(b.Better) != "":
		verdict = "corrected"
	case b.Good:
		verdict = "good"
	}
	noteAudit(r, "rate "+id+" "+verdict, id)
	if strings.TrimSpace(b.Better) == "" {
		b.Better = ""
	}
	if err := s.finetune.RateOwner(id, b.Good, b.Better, strings.TrimSpace(b.Note)); err != nil {
		finetuneErr(w, err)
		return
	}
	s.writeExample(w, id)
}

func (s *server) handleFinetuneExclude(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	id := r.PathValue("id")
	var b struct {
		Exclude *bool `json:"exclude"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	if b.Exclude == nil {
		writeError(w, http.StatusBadRequest, "exclude (true or false) is required")
		return
	}
	noteAudit(r, fmt.Sprintf("exclude %s %v", id, *b.Exclude), id)
	if err := s.finetune.Exclude(id, *b.Exclude); err != nil {
		finetuneErr(w, err)
		return
	}
	s.writeExample(w, id)
}

func (s *server) writeExample(w http.ResponseWriter, id string) {
	it, err := s.finetune.Get(id)
	if err != nil {
		finetuneErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// validBase checks a base model name for the trainer's command line: a
// short single line with no placeholder braces (it is substituted before
// the session's own placeholders are).
func validBase(b string) error {
	if len(b) > 200 {
		return errors.New("base is too long")
	}
	for _, r := range b {
		if unicode.IsControl(r) || r == '{' || r == '}' {
			return errors.New("base must be a plain model name")
		}
	}
	return nil
}

func (s *server) handleFinetuneTrain(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	var b struct {
		Base    string `json:"base"`
		Unrated bool   `json:"unrated"`
	}
	if err := decodeBody(w, r, &b); err != nil {
		writeError(w, bodyErrStatus(err), "invalid body: "+err.Error())
		return
	}
	cfg := s.conf()
	base := strings.TrimSpace(b.Base)
	if base == "" {
		base = strings.TrimSpace(cfg.Finetune.BaseModel)
	}
	if err := validBase(base); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	noteAudit(r, fmt.Sprintf("finetune train base=%s unrated=%v", base, b.Unrated))
	if len(cfg.Finetune.TrainCommand) == 0 || strings.TrimSpace(cfg.Finetune.TrainCommand[0]) == "" {
		writeError(w, http.StatusConflict, "no trainer is set up: set finetune.train_command in Settings")
		return
	}
	ts, ok := s.sessions.(trainService)
	if !ok {
		unavailable(w, "sessions")
		return
	}
	run, err := s.finetune.NewRun(base, finetune.ExportOptions{Unrated: b.Unrated})
	if errors.Is(err, finetune.ErrNoRows) {
		writeError(w, http.StatusConflict, "no usable examples yet: rate some answers good or correct them first")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// {dataset} is the copy in the workspace; the session expands {cwd}.
	argv := finetune.Expand(cfg.Finetune.TrainCommand, filepath.Join("{cwd}", "dataset.jsonl"), base, run.Out)
	for i, a := range argv {
		argv[i] = config.ExpandHome(a)
	}
	title := "Fine-tune"
	if base != "" {
		title += " " + base
	}
	rows := "examples"
	if run.Rows == 1 {
		rows = "example"
	}
	info, err := ts.StartTrain(r.Context(), session.TrainSpec{Title: title,
		Task:    fmt.Sprintf("%s on %d %s (run %s, output in %s)", title, run.Rows, rows, run.ID, run.Out),
		Command: argv, Files: map[string]string{"dataset.jsonl": run.Dataset}})
	if err != nil {
		_ = os.RemoveAll(run.Dir) // nothing ran: no run to list
		status := http.StatusInternalServerError
		if errors.Is(err, session.ErrTrainerMissing) {
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	run.Session, run.Command = info.ID, argv
	if err := s.finetune.SaveRun(run); err != nil {
		s.log.Warn("finetune: run not recorded", "run", run.ID, "err", err)
	}
	noteAudit(r, fmt.Sprintf("finetune train base=%s rows=%d run=%s", base, run.Rows, run.ID), info.ID)
	w.Header().Set("X-Cerberus-Run", run.ID)
	writeJSON(w, http.StatusCreated, info)
}

// runView is a Run with its session's state.
type runView struct {
	finetune.Run
	State string `json:"state"`
}

func (s *server) handleFinetuneRuns(w http.ResponseWriter, r *http.Request) {
	if s.finetune == nil {
		unavailable(w, "finetune")
		return
	}
	runs, err := s.finetune.Runs()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	out := make([]runView, 0, len(runs))
	for _, run := range runs {
		v := runView{Run: run, State: "cli"}
		if run.Session != "" {
			v.State = "removed"
			if s.sessions != nil {
				if info, ok := s.sessions.Get(run.Session); ok {
					v.State = info.State
				}
			}
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}
