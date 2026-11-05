package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"carino.systems/cerberus/internal/alerts"
	"carino.systems/cerberus/internal/api"
	"carino.systems/cerberus/internal/api/devices"
	"carino.systems/cerberus/internal/api/doctor"
	"carino.systems/cerberus/internal/audit"
	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/downloads"
	"carino.systems/cerberus/internal/energy"
	"carino.systems/cerberus/internal/engine"
	"carino.systems/cerberus/internal/files"
	"carino.systems/cerberus/internal/gateway"
	"carino.systems/cerberus/internal/gpu"
	"carino.systems/cerberus/internal/inbox"
	"carino.systems/cerberus/internal/notify"
	"carino.systems/cerberus/internal/permits"
	"carino.systems/cerberus/internal/platform"
	"carino.systems/cerberus/internal/session"
	"carino.systems/cerberus/internal/subs"
	"carino.systems/cerberus/internal/sysmon"
	"carino.systems/cerberus/internal/usage"
	"carino.systems/cerberus/internal/users"
	"carino.systems/cerberus/internal/vault"
	"carino.systems/cerberus/internal/voice"
)

// voiceSecret is the vault name of the bearer token for an OpenAI-compatible
// transcription URL (voice.url); local backends need none.
const voiceSecret = "voice"

const (
	usageKeep       = 1000
	shutdownTimeout = 45 * time.Second // below the unit's TimeoutStopSec
)

func serveCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	setUsage(fs)
	cfgPath := fs.String("config", "", "config file (default ~/.config/cerberus/config.json)")
	debug := fs.Bool("debug", false, "debug logging")
	if fs.Parse(args) != nil {
		return 2
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := serve(ctx, stop, *cfgPath, log, nil)
	if errors.Is(err, errRestart) {
		return restartSelf(log)
	}
	if err != nil {
		log.Error("serve failed", "err", err)
		return 1
	}
	return 0
}

// serve runs the daemon until ctx ends (SIGINT or SIGTERM). stop releases
// the signal handlers once shutdown begins; ready (tests) gets the bound
// addresses.
func serve(ctx context.Context, stop func(), cfgPath string, log *slog.Logger, ready func([]string)) error {
	if home, err := os.UserHomeDir(); err == nil {
		_ = os.Setenv("PATH", withUserBins(os.Getenv("PATH"), home))
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	// POST /api/config/restart ends this run like a signal would, then
	// serveCmd starts the daemon again (restart.go).
	ctx, requestRestart := context.WithCancel(ctx)
	defer requestRestart()
	var restarting atomic.Bool
	if err := bindSafety(cfg.Server, log); err != nil {
		return err
	}
	data, err := platform.DataDir()
	if err != nil {
		return err
	}
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()

	store, err := usage.Open(filepath.Join(data, "usage.jsonl"), usageKeep)
	if err != nil {
		return err
	}
	defer store.Close()
	// A vault that cannot open (a forced backend that is absent, a key that
	// does not match) is reported, not fatal: the daemon runs without it and
	// /api/status shows no vault backend.
	vlt, err := vault.Open(cfg, log.With("part", "vault"))
	if err != nil {
		log.Error("vault unavailable; provider keys fall back to key_env and secrets.json", "err", err)
		vlt = nil
	}
	var secretValues func() []string
	if vlt != nil {
		secretValues = vlt.Values
	}
	auditLog, err := audit.Open(data, audit.Options{Secrets: secretValues})
	if err != nil {
		return fmt.Errorf("audit log: %w", err)
	}
	defer auditLog.Close()
	_ = auditLog.Event(audit.ActorSystem, "serve.start", "cerberus "+version+" on "+strings.Join(append([]string{cfg.Server.Listen}, cfg.Server.ExtraListen...), " "))
	// A devices.json that cannot be read fails closed: no device token works
	// until it is fixed (the admin token still does).
	paired, err := devices.Open(cfg.Dir())
	if err != nil {
		log.Error("paired devices unavailable; only the admin token works", "err", err)
		paired = nil
	}
	// A users.json that cannot be read fails closed: no password sign-in
	// works until it is fixed (the admin token still does).
	accounts, err := users.Open(data)
	if err != nil {
		return fmt.Errorf("user accounts (%s): %w; fix the file or restore it from a backup (the daemon refuses to start without it once accounts existed)", filepath.Join(data, users.FileName), err)
	}
	// Saved permissions ("Always allow"): a file that cannot be read is
	// reported, and nothing is saved or matched until it is fixed (never
	// overwritten).
	saved, err := permits.Open(data)
	if err != nil {
		log.Error("saved permissions unavailable; every approval asks", "err", err)
		saved = nil
	} else {
		// Only the single owner from before users ("") resolves (to the
		// first admin); a removed user's rules are not handed to whoever
		// inherits their sessions: a yes is personal.
		saved.SetResolver(func(id string) string {
			if id == "" {
				return accounts.Owner("")
			}
			return id
		})
		defer func() { _ = saved.Flush() }()
	}
	var engOpts []engine.Option
	sessOpts := []session.Option{session.WithAudit(auditLog), session.WithPermits(saved), session.WithConfine(func(owner string) bool {
		// Recomputed at every run from the owner's current role (never only
		// from the session's own, agent-writable folder).
		if !accounts.HasUsers() {
			return false
		}
		u, ok := accounts.Get(accounts.Owner(owner))
		return !ok || u.Disabled || u.Role != users.RoleAdmin
	})}
	voiceKey := func() string { return "" }
	if vlt != nil {
		engOpts = append(engOpts, engine.WithSecrets(vlt.Lookup))
		sessOpts = append(sessOpts, session.WithVault(vlt))
		voiceKey = func() string { return vlt.Lookup(voiceSecret) }
	}
	notifier := notify.New(cfg.Notify, log.With("part", "notify"))
	// The in-app inbox keeps every notification; one that cannot open is
	// reported, not fatal (notifications still go out).
	notes, err := inbox.Open(filepath.Join(data, "inbox.jsonl"), 0)
	if err != nil {
		log.Error("inbox unavailable; notifications are not kept in-app", "err", err)
		notes = nil
	} else {
		notifier.SetInbox(notes)
		defer notes.Close()
	}
	mon := gpu.New(cfg.GPU, log.With("part", "gpu"))
	eng := engine.NewManager(cfg, mon, log.With("part", "engine"), engOpts...)
	// Energy is created before the sessions (they read their cost from it) and
	// learns the live sessions afterwards; sess is set before anything samples.
	var sess *session.Manager
	acct := energy.New(cfg, filepath.Join(data, "energy"), log.With("part", "energy"),
		energy.WithUsage(store.Aggregate),
		energy.WithSessions(func() map[string]int {
			if sess == nil {
				return nil
			}
			return sess.LivePIDs()
		}),
		energy.WithKnown(func(id string) bool {
			if sess == nil {
				return true
			}
			_, ok := sess.Get(id)
			return ok
		}))
	store.OnAdd(acct.OnUsage)
	sessOpts = append(sessOpts, session.WithCost(acct.SessionCost), session.WithForget(acct.Forget))
	sess, err = session.NewManager(cfg, eng, mon, notifier, log.With("part", "session"), sessOpts...)
	if err != nil {
		return err
	}
	sysmonitor := sysmon.New(cfg, mon, log.With("part", "sysmon"), sysmon.WithAudit(auditLog))
	sysmonitor.SetPowerFunc(acct.Sample)
	alerter := alerts.New(cfg, acct, notifier.Send)
	sysmonitor.AddHook(alerter.Check)
	chatCfg := newLiveConfig(cfg)
	ft, err := openFinetune(cfg, data, secretValues)
	if err != nil {
		log.Error("fine-tuning examples unavailable", "err", err)
		ft = nil
	}
	gw := gateway.New(eng, mon, store, log.With("part", "gateway"), gateway.WithConfig(cfg), gateway.WithNotifier(notifier.Send),
		gateway.WithExchange(func() bool { return ft != nil && chatCfg.Load().Finetune.Capture == config.CaptureCloud },
			func(ex gateway.Exchange) { keepExchange(ft, ex, log) }))
	tracker := subs.New(cfg, log.With("part", "subs"))
	plans := newPlanActivity(tracker)
	transcriber := voice.New(cfg.Voice, log.With("part", "voice"), voice.WithAPIKey(voiceKey))
	dropped := files.New(cfg, sess, vlt, log.With("part", "files"))
	chats, err := newChats(cfg, chatCfg, eng, notifier, data, accounts, saved, log.With("part", "chat"))
	if err != nil {
		return err
	}
	dlFeed := api.NewDownloadFeed()
	pulls := newDownloads(chatCfg, notifier, dlFeed, log.With("part", "downloads"))
	flowFeed := api.NewFlowFeed()
	flowPush := notify.New(cfg.Notify, log.With("part", "notify")) // desktop/push only: approvals already have their inbox item
	flowEng := openFlows(flowsSetup{data: data, cfg: cfg, live: chatCfg, gw: gw, usage: store, sess: sess, eng: eng,
		notifier: notifier, push: flowPush, inbox: notes, audit: auditLog, feed: flowFeed, users: accounts}, log.With("part", "flows"))

	var bg sync.WaitGroup
	for _, fn := range []func(context.Context){mon.Run, eng.Run, sess.Run, tracker.Run, acct.Run, sysmonitor.Run, dropped.Run} {
		bg.Go(func() { fn(runCtx) })
	}
	bg.Go(func() { plans.followSessions(runCtx, sess, subscriptionAgents(cfg)) })

	deps := api.Deps{Cfg: cfg, Engine: eng, GPU: mon, Usage: store, Sessions: sess, Log: log,
		Gateway: gw, Version: version, Build: build, Subs: plans, Voice: transcriber, Sysmon: sysmonitor, Energy: acct, Files: dropped,
		Audit: auditLog, Devices: paired, DataDir: data, Finetune: ft,
		// Live reload: each gets the new config after a PATCH /api/config.
		Chat:      chats,
		Downloads: pulls, DownloadFeed: dlFeed,
		Reloaders: []api.Reloader{eng, gw, notifier, sysmonitor, acct, alerter, sess, dropped, chatCfg, flowPush, api.FlowReloader(flowEng),
			api.UsersReloader(accounts)},
		Users:    accounts,
		Permits:  saved,
		FlowFeed: flowFeed,
		Restart: func() {
			restarting.Store(true)
			requestRestart()
		},
		RestartCheck: func() error { return restartPreflight(cfg.Path(), cfg.Server.Listen) }}
	if vlt != nil {
		deps.Vault = vlt // a nil *vault.Vault must not become a non-nil interface downstream
	}
	if notes != nil {
		deps.Inbox = notes
	}
	deps.Flows = flowEng // nil when the engine did not open: the routes answer 503
	// Work previews: the listeners' TLS config is known once listenAll ran,
	// and no preview port is bound before a request asks for one.
	var listeners *listenSet
	previews, err := api.NewPreviews(cfg.Server.PreviewPorts, func(port int) ([]net.Listener, error) {
		return bindPreview(cfg.Server, listeners, port)
	}, log.With("part", "preview"))
	if err != nil {
		log.Error("server.preview_ports ignored", "err", err)
	}
	deps.Previews = previews
	handler := api.New(deps)
	listeners, addrs, err := listenAll(cfg.Server, handler, log)
	if err != nil {
		closeFlows(flowEng)
		pulls.Close()
		chats.Close()
		sess.Shutdown(context.Background())
		return err
	}
	if previews != nil {
		bg.Go(func() { previews.Run(runCtx) })
	}
	log.Info("cerberus ready", "version", version, "listen", cfg.Server.Listen, "data", data)
	if flowEng != nil {
		flowEng.Start() // the scheduler, once sessions can reach the gateway
	}
	if ready != nil {
		ready(addrs)
	}
	<-ctx.Done()
	stop() // a second signal now kills the process the default way
	log.Info("shutting down")

	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Chats stop first: a native chat's loopback /v1 request cut by the
	// listeners' shutdown then ends as stopped, not failed (no notification,
	// no queued message started against a closed gateway).
	chats.Stopping()
	previews.Close()
	listeners.shutdown(sctx)
	// Flow runs stop before the sessions: running runs are recorded as
	// failed ("daemon restarted"); runs waiting for an approval keep
	// waiting; the sessions they started stop with the others below.
	closeFlows(flowEng)
	chats.Close() // waits for the runs (their commands' process groups end too)
	pulls.Close() // cancels downloads; partial GGUF files stay to resume
	sess.Shutdown(sctx)
	eng.Shutdown(sctx)
	cancelRun()
	bg.Wait()
	if restarting.Load() {
		_ = auditLog.Event(audit.ActorSystem, "serve.restart", "restarting on request")
		return errRestart
	}
	return nil
}

// newDownloads is the model download manager: progress goes to feed (the
// event stream) and a finished download becomes a notification, hence an
// inbox item. It reads the live config, so engine changes apply at once.
func newDownloads(live *liveConfig, n *notify.Notifier, feed *api.DownloadFeed, log *slog.Logger) *downloads.Manager {
	return downloads.New(downloads.Options{Config: live.Load, Log: log, OnChange: feed.Publish,
		OnDone: func(it downloads.Item) { n.Send(downloadMessage(it)) }})
}

// downloadMessage turns a finished download into a notification.
func downloadMessage(it downloads.Item) notify.Message {
	kind, level, title, body, link := it.Notice()
	// SessionID only keys the rate limit, so two downloads finishing
	// together both notify; Kind and Link are set, so it is not read as a
	// session.
	return notify.Message{Title: title, Body: body, Urgency: "normal", Kind: kind, Level: level, Link: link,
		SessionID: "download:" + it.ID}
}

// bindSafety refuses public listen addresses unless server.allow_public is
// set, and warns loudly about wildcard binds (doctor.CheckListen).
func bindSafety(sc config.ServerConfig, log *slog.Logger) error {
	warns, err := doctor.CheckListen(append([]string{sc.Listen}, sc.ExtraListen...), sc.AllowPublic, resolveHost)
	for _, w := range warns {
		log.Warn("SECURITY WARNING: " + w)
	}
	return err
}

// restartPreflight checks that the daemon would start again with the
// config file at path, as a restart would load it: the file parses, the
// listen addresses pass bind safety (bindSafety), and a primary listen
// address other than the one bound now (current) can be bound. Extra
// addresses are retried after start, so only the primary must bind.
func restartPreflight(path, current string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	next, err := config.Parse(path, raw)
	if err != nil {
		return err
	}
	if err := bindSafety(next.Server, slog.New(slog.DiscardHandler)); err != nil {
		return err
	}
	if next.Server.Listen == current {
		return nil
	}
	ln, err := net.Listen("tcp", next.Server.Listen)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", next.Server.Listen, err)
	}
	defer ln.Close()
	return doctor.BoundSafe(ln.Addr(), next.Server.AllowPublic)
}

// resolveHost looks up a listen host name (bounded, so a dead resolver
// cannot stall startup).
func resolveHost(host string) ([]net.IP, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	ips := make([]net.IP, len(addrs))
	for i, a := range addrs {
		ips[i] = a.IP
	}
	return ips, err
}

// Extra-listen retry backoff; tests shorten it.
var (
	retryMin = 5 * time.Second
	retryMax = 60 * time.Second
)

// listenSet owns every HTTP server: the primary Listen (plain HTTP) and each
// ExtraListen (HTTPS when TLSCert and TLSKey are set). An extra address that
// cannot bind yet (Tailscale not up at boot) is retried in the background.
type listenSet struct {
	h   http.Handler
	log *slog.Logger
	tls *tls.Config // nil: extras serve plain HTTP
	// allowPublic is Server.AllowPublic: without it, a listener whose
	// bound address is public is closed (doctor.BoundSafe).
	allowPublic bool
	ctx         context.Context
	cancel      context.CancelFunc
	wg          sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	servers map[*http.Server]struct{}
}

// listenAll binds Server.Listen (fatal on error) and tries each ExtraListen
// once; those that fail keep retrying with backoff until shutdown. The
// returned addresses are the ones bound now.
func listenAll(sc config.ServerConfig, h http.Handler, log *slog.Logger) (*listenSet, []string, error) {
	ln, err := net.Listen("tcp", sc.Listen)
	if err != nil {
		return nil, nil, fmt.Errorf("listen %s: %w", sc.Listen, err)
	}
	if err := doctor.BoundSafe(ln.Addr(), sc.AllowPublic); err != nil {
		ln.Close()
		return nil, nil, fmt.Errorf("listen %s: %w", sc.Listen, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ls := &listenSet{h: h, log: log, ctx: ctx, cancel: cancel, servers: map[*http.Server]struct{}{},
		allowPublic: sc.AllowPublic}
	if len(sc.ExtraListen) > 0 && sc.TLSCert != "" && sc.TLSKey != "" {
		cr := newCertReloader(sc.TLSCert, sc.TLSKey, log.With("part", "tls"))
		ls.tls = cr.tlsConfig()
		ls.wg.Go(func() { cr.watch(ctx) })
	}
	addrs := []string{ln.Addr().String()}
	ls.start(ln, false)
	for _, addr := range sc.ExtraListen {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			log.Warn("extra listen address unavailable, retrying", "addr", addr, "err", err)
			ls.wg.Go(func() { ls.retry(addr) })
			continue
		}
		if err := doctor.BoundSafe(ln.Addr(), ls.allowPublic); err != nil {
			ln.Close()
			log.Error("SECURITY: extra listen address not served", "addr", addr, "err", err)
			continue
		}
		addrs = append(addrs, ln.Addr().String())
		ls.start(ln, ls.tls != nil)
	}
	return ls, addrs, nil
}

// start serves ln in the background, unless shutdown has begun.
func (ls *listenSet) start(ln net.Listener, useTLS bool) {
	srv := newHTTPServer(ls.h, ls.log)
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		ln.Close()
		return
	}
	ls.servers[srv] = struct{}{}
	ls.mu.Unlock()
	addr := ln.Addr().String()
	ls.log.Info("listening", "addr", addr, "tls", useTLS)
	ls.wg.Go(func() {
		var err error
		if useTLS {
			srv.TLSConfig = ls.tls
			err = srv.ServeTLS(ln, "", "")
		} else {
			err = srv.Serve(ln)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			ls.log.Error("http server", "addr", addr, "err", err)
		}
	})
}

// retry binds addr with exponential backoff until it succeeds or shutdown
// begins. It logs only when the error changes, so a long wait stays quiet.
func (ls *listenSet) retry(addr string) {
	delay := retryMin
	lastErr := ""
	for {
		t := time.NewTimer(delay)
		select {
		case <-ls.ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		ln, err := net.Listen("tcp", addr)
		if err == nil {
			// The name may resolve now to something it did not at startup
			// (bind safety could not judge it then): check what was bound.
			if err := doctor.BoundSafe(ln.Addr(), ls.allowPublic); err != nil {
				ln.Close()
				ls.log.Error("SECURITY: extra listen address not served", "addr", addr, "err", err)
				return
			}
			ls.log.Info("extra listen address bound after retry", "addr", addr)
			ls.start(ln, ls.tls != nil)
			return
		}
		if msg := err.Error(); msg != lastErr {
			lastErr = msg
			ls.log.Warn("extra listen address still unavailable", "addr", addr, "err", err, "next", delay)
		}
		delay = min(delay*2, retryMax)
	}
}

// shutdown stops the retry and reload goroutines, shuts every server down
// and waits for all of them to return.
func (ls *listenSet) shutdown(ctx context.Context) {
	ls.cancel()
	ls.mu.Lock()
	ls.closed = true
	servers := make([]*http.Server, 0, len(ls.servers))
	for srv := range ls.servers {
		servers = append(servers, srv)
	}
	ls.mu.Unlock()
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil {
			ls.log.Warn("http shutdown", "err", err)
		}
	}
	ls.wg.Wait()
}

// newHTTPServer has no WriteTimeout (SSE and model streams are long-lived).
// Request contexts derive from a base context that Shutdown cancels, so open
// streams end promptly instead of holding shutdown until the timeout.
func newHTTPServer(h http.Handler, log *slog.Logger) *http.Server {
	base, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelDebug),
	}
	srv.RegisterOnShutdown(cancel)
	return srv
}

// withUserBins puts the user's own bin folders (~/.local/bin, ~/bin) on
// PATH when they exist and are missing. A systemd user manager runs with a
// bare PATH, and agents such as claude live in ~/.local/bin; agent sessions
// inherit this PATH too.
func withUserBins(path, home string) string {
	have := map[string]bool{}
	for _, d := range filepath.SplitList(path) {
		have[filepath.Clean(d)] = true
	}
	var add []string
	for _, d := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin")} {
		if st, err := os.Stat(d); err == nil && st.IsDir() && !have[d] {
			add = append(add, d)
		}
	}
	if len(add) == 0 {
		return path
	}
	if path != "" {
		add = append(add, path)
	}
	return strings.Join(add, string(filepath.ListSeparator))
}

// bindPreview binds one preview port on every distinct host of listen and
// extra_listen: plain HTTP where the app is plain, HTTPS (the app's
// certificate) on extra addresses when TLS is configured. A host bound
// both ways takes the first (listen). Public addresses are refused unless
// server.allow_public, as for the app. Any failure closes what was bound.
func bindPreview(sc config.ServerConfig, ls *listenSet, port int) ([]net.Listener, error) {
	type hostTLS struct {
		host string
		tls  bool
	}
	var hosts []hostTLS
	seen := map[string]bool{}
	for i, a := range append([]string{sc.Listen}, sc.ExtraListen...) {
		h, _, err := net.SplitHostPort(a)
		if err != nil || seen[h] {
			continue
		}
		seen[h] = true
		hosts = append(hosts, hostTLS{h, i > 0 && ls != nil && ls.tls != nil})
	}
	var out []net.Listener
	fail := func(err error) ([]net.Listener, error) {
		for _, ln := range out {
			ln.Close()
		}
		return nil, err
	}
	for _, h := range hosts {
		ln, err := net.Listen("tcp", net.JoinHostPort(h.host, strconv.Itoa(port)))
		if err != nil {
			return fail(err)
		}
		if err := doctor.BoundSafe(ln.Addr(), sc.AllowPublic); err != nil {
			ln.Close()
			return fail(err)
		}
		if h.tls {
			ln = tls.NewListener(ln, ls.tls)
		}
		out = append(out, ln)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no listen host to bind preview port %d on", port)
	}
	return out, nil
}
