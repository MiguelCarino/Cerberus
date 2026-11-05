package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/bench"
)

// The bench (gateway phase G6): list runs, start one in the background
// against this daemon's own gateway, read one.

type benchJobs struct {
	mu      sync.Mutex
	running *bench.Run
}

func (s *server) benchDir() string { return filepath.Join(s.dataDir, "bench") }

// handleBenchList answers GET /api/bench: saved runs, newest first, and the
// one running now.
func (s *server) handleBenchList(w http.ResponseWriter, r *http.Request) {
	runs, err := bench.List(s.benchDir())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if runs == nil {
		runs = []bench.Run{}
	}
	s.bench.mu.Lock()
	var cur *bench.Run
	if s.bench.running != nil {
		c := *s.bench.running
		cur = &c
	}
	s.bench.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs, "running": cur, "kinds": bench.Kinds})
}

// handleBenchGet answers GET /api/bench/{id}.
func (s *server) handleBenchGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.bench.mu.Lock()
	if cur := s.bench.running; cur != nil && cur.ID == id {
		c := *cur
		s.bench.mu.Unlock()
		writeJSON(w, http.StatusOK, c)
		return
	}
	s.bench.mu.Unlock()
	if strings.ContainsAny(id, `/\`) {
		writeError(w, http.StatusNotFound, "no such bench run")
		return
	}
	run, err := bench.Get(s.benchDir(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "no such bench run")
		return
	}
	writeJSON(w, http.StatusOK, run)
}

// handleBenchStart answers POST /api/bench {model, repeat, kinds, prefill,
// label, force}: one run at a time, in the background.
func (s *server) handleBenchStart(w http.ResponseWriter, r *http.Request) {
	var in struct {
		bench.Options
		Force bool `json:"force"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
		return
	}
	cfg := s.conf()
	o := in.Options
	if o.Model == "" {
		o.Model = cfg.Engines.DefaultModel
	}
	if o.Model == "" {
		writeError(w, http.StatusBadRequest, "model is required (no engines.default_model)")
		return
	}
	if o.Repeat <= 0 || o.Repeat > 20 {
		o.Repeat = 3
	}
	c := bench.Client{Base: "http://" + loopbackListen(cfg.Server.Listen), Token: cfg.Server.Token,
		HTTP: &http.Client{Timeout: 30 * time.Minute}}
	if !in.Force {
		if busy, err := c.Busy(r.Context()); err == nil && busy {
			writeError(w, http.StatusConflict, bench.ErrBusy.Error())
			return
		}
	}
	set, err := bench.Load()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	run := bench.NewRun(cfg, o, s.version)
	s.bench.mu.Lock()
	if s.bench.running != nil {
		s.bench.mu.Unlock()
		writeError(w, http.StatusConflict, "a bench run is already going: "+s.bench.running.ID)
		return
	}
	s.bench.running = run
	s.bench.mu.Unlock()
	noteAudit(r, "bench "+o.Model+" "+orDefault(o.Label, ""), run.ID)
	go s.runBench(c, set, run)
	writeJSON(w, http.StatusAccepted, map[string]string{"id": run.ID})
}

func (s *server) runBench(c bench.Client, set bench.Set, run *bench.Run) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()
	work := *run
	err := bench.Execute(ctx, c, set, work.Options, &work, func(p string) {
		s.bench.mu.Lock()
		run.Progress = p
		s.bench.mu.Unlock()
	})
	work.Done = time.Now()
	work.State, work.Progress = "done", ""
	if err != nil {
		work.State, work.Error = "failed", err.Error()
	}
	if serr := bench.Save(s.benchDir(), &work); serr != nil && !errors.Is(serr, context.Canceled) {
		s.log.Warn("bench: saving the run failed", "err", serr)
	}
	s.bench.mu.Lock()
	s.bench.running = nil
	s.bench.mu.Unlock()
}

// loopbackListen is the main listen address as this host reaches it.
func loopbackListen(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:7340"
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
