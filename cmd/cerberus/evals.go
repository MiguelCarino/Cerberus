package main

import (
	"log/slog"
	"path/filepath"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/evals"
)

// newEvalHost is the eval engine's daemon side (api.EvalHost). It is made
// before the flow engine opens: flows' judge steps run through it too.
func newEvalHost(s flowsSetup, feed *api.EvalFeed) *api.EvalHost {
	d := api.EvalHostDeps{Config: s.live.Load, Usage: s.usage, Audit: s.audit, Feed: feed, Users: s.users}
	if s.gw != nil {
		d.Gateway = s.gw.Handler()
	}
	if s.sess != nil {
		d.Sessions = s.sess
	}
	if s.eng != nil {
		d.Models = s.eng.Models
	}
	return api.NewEvalHost(d)
}

// openEvals opens the eval engine over the host, with its data in
// DATA/evals. An engine that cannot open is reported, not fatal: the
// daemon runs without evals (the routes answer 503) and nil is returned.
func openEvals(h *api.EvalHost, data string, log *slog.Logger) *evals.Engine {
	o := h.Options(filepath.Join(data, "evals"))
	o.Log = log
	e, err := evals.Open(o)
	if err != nil {
		log.Error("evals unavailable", "err", err)
		return nil
	}
	return e
}

// closeEvals stops the runs going (nil: nothing to do). Their sessions
// are left to the session manager's own shutdown.
func closeEvals(e *evals.Engine) {
	if e != nil {
		_ = e.Close()
	}
}
