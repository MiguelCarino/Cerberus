package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"carino.systems/cerberus/internal/guardbench"
)

// The agent-guard red-team suite (evals phase E5): list runs and the last
// run's two rates, or start one in the background. A run drives every
// cases.json case through the G2 guard classifier path
// (guardScreener.ScreenText), so loading the guard model is the owner's
// step. Admins only.

// guardScreener is the guard classifier path (internal/gateway.Gateway);
// a run calls it per case. Tests inject a fake.
type guardScreener interface {
	ScreenText(ctx context.Context, model, text string) (float64, string, error)
}

type guardbenchJobs struct {
	mu      sync.Mutex
	running *guardbench.Run
}

func (s *server) guardbenchDir() string { return filepath.Join(s.dataDir, "guardbench") }

// handleGuardbenchList answers GET /api/guardbench: saved runs (newest
// first), the one running now, the last done run's rates, and the case
// categories.
func (s *server) handleGuardbenchList(w http.ResponseWriter, r *http.Request) {
	runs, err := guardbench.List(s.guardbenchDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []guardbench.Run{}
	}
	s.guardbench.mu.Lock()
	var cur *guardbench.Run
	if s.guardbench.running != nil {
		c := *s.guardbench.running
		cur = &c
	}
	s.guardbench.mu.Unlock()
	var last *guardbench.Run
	for i := range runs {
		if runs[i].State == "done" {
			last = &runs[i]
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "running": cur, "last": last,
		"attack_categories": guardbench.AttackCategories, "benign_categories": guardbench.BenignCategories})
}

// handleGuardbenchStart answers POST /api/guardbench {model, threshold}:
// one run at a time, in the background, through the guard classifier path.
func (s *server) handleGuardbenchStart(w http.ResponseWriter, r *http.Request) {
	var in guardbench.Options
	// An empty body means "use the configured model and threshold".
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	if s.guardScreen == nil {
		writeError(w, http.StatusServiceUnavailable, "the gateway is not available")
		return
	}
	cfg := s.conf()
	if in.Model == "" {
		in.Model = cfg.Gateway.Guard.Model
	}
	if in.Model == "" {
		writeError(w, http.StatusBadRequest, guardbench.ErrNoModel.Error())
		return
	}
	if in.Threshold <= 0 {
		in.Threshold = cfg.Gateway.Guard.Threshold
	}
	set, err := guardbench.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run := guardbench.NewRun(in, s.version)
	s.guardbench.mu.Lock()
	if s.guardbench.running != nil {
		id := s.guardbench.running.ID
		s.guardbench.mu.Unlock()
		writeError(w, http.StatusConflict, "a guard bench run is already going: "+id)
		return
	}
	s.guardbench.running = run
	s.guardbench.mu.Unlock()
	noteAudit(r, "guard bench "+in.Model, run.ID)
	go s.runGuardbench(set, run)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": run.ID})
}

func (s *server) runGuardbench(set guardbench.Set, run *guardbench.Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	work := *run
	err := guardbench.Execute(ctx, s.guardScreen, set, work.Options, &work, func(p string) {
		s.guardbench.mu.Lock()
		run.Progress = p
		s.guardbench.mu.Unlock()
	})
	work.Done = time.Now()
	work.State, work.Progress = "done", ""
	if err != nil {
		work.State, work.Error = "failed", err.Error()
	}
	if serr := guardbench.Save(s.guardbenchDir(), &work); serr != nil {
		s.log.Warn("guardbench: saving the run failed", "err", serr)
	}
	s.guardbench.mu.Lock()
	s.guardbench.running = nil
	s.guardbench.mu.Unlock()
}
