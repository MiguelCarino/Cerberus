package main

import (
	"log/slog"
	"path/filepath"

	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/flows"
	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
)

// flowsSetup is what the daemon hands the flow engine (serve).
type flowsSetup struct {
	data     string         // DATA: flows live in DATA/flows
	cfg      *config.Config // the config at start (flows.enabled, flows.max_runs)
	live     *liveConfig    // the live config, for the engine's dependencies
	gw       *gateway.Gateway
	usage    *usage.Store
	sess     *session.Manager
	eng      *engine.Manager
	notifier *notify.Notifier // notifications, kept in the inbox
	push     *notify.Notifier // desktop/push only (approval reminders)
	inbox    *inbox.Store     // nil when the inbox did not open
	audit    *audit.Logger
	feed     *api.FlowFeed
	users    *users.Store  // accounts (Phase 4a); nil: single owner
	judge    *api.EvalHost // judge steps (evals E2); nil: they fail
}

// openFlows opens the flow engine over the daemon's services (api.FlowHost).
// The scheduler is not started yet: serve starts it once the listeners are
// up. An engine that cannot open is reported, not fatal: the daemon runs
// without flows (the routes answer 503) and nil is returned.
func openFlows(s flowsSetup, log *slog.Logger) *flows.Engine {
	d := api.FlowHostDeps{Config: s.live.Load, Usage: s.usage, Audit: s.audit, Feed: s.feed, Users: s.users}
	if s.gw != nil {
		d.Gateway = s.gw.Handler()
	}
	if s.sess != nil {
		d.Sessions = s.sess
	}
	if s.eng != nil {
		d.Models = s.eng.Models
	}
	if s.notifier != nil {
		d.Send = s.notifier.Send
	}
	if s.push != nil {
		d.Push = s.push.Send
	}
	if s.inbox != nil {
		d.Inbox = s.inbox
	}
	if s.judge != nil {
		d.Judge = s.judge // a nil *EvalHost must not become a non-nil interface
	}
	o := api.NewFlowHost(d).Options(filepath.Join(s.data, "flows"))
	o.MaxRuns = s.cfg.Flows.MaxRuns
	o.Log = log
	e, err := flows.Open(o)
	if err != nil {
		log.Error("flows unavailable", "err", err)
		return nil
	}
	if !s.cfg.Flows.Enabled {
		e.SetEnabled(false)
	}
	return e
}

// closeFlows stops the engine (nil: nothing to do).
func closeFlows(e *flows.Engine) {
	if e != nil {
		_ = e.Close()
	}
}
