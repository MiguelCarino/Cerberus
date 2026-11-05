// Package notify delivers session notifications through optional channels:
// a command hook (Atrium's scripts/notify by default), an ntfy topic, and
// notify-send as a desktop fallback.
//
// Send never blocks or fails the caller: delivery runs in a goroutine with a
// timeout, and repeats are rate-limited per session and title.
//
// Every message that passes the rate limit is also stored in the in-app
// inbox (SetInbox), with a Kind, a Level and an in-app Link derived from the
// message when the sender leaves them empty. The inbox keeps notifications
// even when the outside channels are switched off (notify.disabled).
package notify

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/inbox"
)

const (
	sendTimeout = 10 * time.Second
	rateWindow  = 10 * time.Second
	maxKeys     = 256 // prune the rate-limit map past this many entries
)

// Message is one notification. Urgency is low, normal or critical. URL is
// an absolute link for outside channels (ntfy click); Link is the in-app
// route ("#/s/<id>") for the inbox. Kind (session, chat, budget, alert,
// doctor, download, system) and Level (info, warn, crit) are optional:
// empty ones are inferred (see Classify).
type Message struct {
	Title     string
	Body      string
	Urgency   string
	SessionID string
	URL       string

	// Day phase 6 (inbox); add-only.
	Kind  string
	Level string
	Link  string
}

// Inbox stores notifications in-app (*inbox.Store).
type Inbox interface {
	Add(inbox.Item) inbox.Item
}

// Notifier fans a message out to the configured channels.
type Notifier struct {
	cfgMu      sync.RWMutex // guards cfg and notifySend (Reload)
	cfg        config.NotifyConfig
	log        *slog.Logger
	notifySend string // resolved path, "" if absent

	// Injectable for tests.
	now    func() time.Time
	run    func(ctx context.Context, argv []string) error
	client *http.Client

	mu   sync.Mutex
	last map[string]time.Time
	wg   sync.WaitGroup

	inboxMu sync.RWMutex
	inbox   Inbox
}

// SetInbox makes every sent message also land in ib (nil turns it off).
func (n *Notifier) SetInbox(ib Inbox) {
	if n == nil {
		return
	}
	n.inboxMu.Lock()
	n.inbox = ib
	n.inboxMu.Unlock()
}

func (n *Notifier) store(msg Message) { n.storeFor(msg, "") }

func (n *Notifier) storeFor(msg Message, owner string) {
	n.inboxMu.RLock()
	ib := n.inbox
	n.inboxMu.RUnlock()
	if ib == nil {
		return
	}
	kind, level, link := Classify(msg)
	ib.Add(inbox.Item{Kind: kind, Level: level, Title: msg.Title, Body: msg.Body, Link: link, Owner: owner})
}

// Keep puts msg in the inbox for one user only (Phase 4a), with no desktop,
// push or command-hook notification: those are the admin's, while other
// users get inbox items.
func (n *Notifier) Keep(msg Message, owner string) {
	if n == nil {
		return
	}
	n.storeFor(msg, owner)
}

// New builds a notifier. notify-send is looked up once, here.
func New(cfg config.NotifyConfig, log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	n := &Notifier{
		cfg:    cfg,
		log:    log,
		now:    time.Now,
		run:    runCommand,
		client: &http.Client{Timeout: sendTimeout},
		last:   map[string]time.Time{},
	}
	n.notifySend = lookNotifySend(cfg)
	return n
}

// lookNotifySend resolves notify-send when no command hook is set.
func lookNotifySend(cfg config.NotifyConfig) string {
	if len(cfg.Command) == 0 {
		if p, err := exec.LookPath("notify-send"); err == nil {
			return p
		}
	}
	return ""
}

// Reload replaces the channels (day phase 4 live reload): the command hook,
// the ntfy topic and the off switch apply to the next Send.
func (n *Notifier) Reload(cfg *config.Config) {
	if n == nil || cfg == nil {
		return
	}
	nc := cfg.Notify
	nc.Command = slices.Clone(nc.Command)
	ns := lookNotifySend(nc)
	n.cfgMu.Lock()
	n.cfg, n.notifySend = nc, ns
	n.cfgMu.Unlock()
}

func (n *Notifier) conf() config.NotifyConfig {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.cfg
}

func (n *Notifier) sendPath() string {
	n.cfgMu.RLock()
	defer n.cfgMu.RUnlock()
	return n.notifySend
}

// Send delivers msg in the background. It returns immediately.
func (n *Notifier) Send(msg Message) {
	if n == nil {
		return
	}
	if !n.allow(msg.SessionID + "\x00" + msg.Title) {
		return
	}
	msg.Urgency = normUrgency(msg.Urgency)
	n.store(msg)
	c := n.conf()
	if c.Disabled {
		return
	}
	n.wg.Go(func() { n.deliver(c, msg) })
}

// allow reports whether key may send now and records the send.
func (n *Notifier) allow(key string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.now()
	if t, ok := n.last[key]; ok && now.Sub(t) < rateWindow {
		return false
	}
	if len(n.last) >= maxKeys {
		for k, t := range n.last {
			if now.Sub(t) >= rateWindow {
				delete(n.last, k)
			}
		}
	}
	n.last[key] = now
	return true
}

func (n *Notifier) deliver(c config.NotifyConfig, msg Message) {
	ctx, cancel := context.WithTimeout(context.Background(), sendTimeout)
	defer cancel()
	if argv := n.commandArgv(c, msg); argv != nil {
		if err := n.run(ctx, argv); err != nil {
			n.log.Debug("notify command failed", "cmd", argv[0], "err", err)
		}
	}
	if c.NtfyURL != "" {
		if err := n.postNtfy(ctx, c.NtfyURL, msg); err != nil {
			n.log.Debug("ntfy failed", "err", err)
		}
	}
}

// commandArgv returns the command hook argv, or notify-send's, or nil.
func (n *Notifier) commandArgv(c config.NotifyConfig, msg Message) []string {
	if len(c.Command) > 0 {
		r := strings.NewReplacer("{title}", msg.Title, "{body}", msg.Body, "{urgency}", msg.Urgency)
		argv := make([]string, len(c.Command))
		for i, a := range c.Command {
			argv[i] = r.Replace(a)
		}
		return argv
	}
	if ns := n.sendPath(); ns != "" {
		return []string{ns, "-a", "Cerberus", "-u", msg.Urgency, msg.Title, msg.Body}
	}
	return nil
}

func (n *Notifier) postNtfy(ctx context.Context, url string, msg Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(msg.Body))
	if err != nil {
		return err
	}
	req.Header.Set("Title", headerSafe(msg.Title))
	req.Header.Set("Priority", ntfyPriority(msg.Urgency))
	if msg.URL != "" {
		req.Header.Set("Click", headerSafe(msg.URL))
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return &statusError{resp.StatusCode}
	}
	return nil
}

type statusError struct{ code int }

func (e *statusError) Error() string { return "ntfy: HTTP " + strconv.Itoa(e.code) }

// runCommand executes argv directly (no shell) with output discarded.
func runCommand(ctx context.Context, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	return cmd.Run()
}

func normUrgency(u string) string {
	switch u {
	case "low", "critical":
		return u
	}
	return "normal"
}

func ntfyPriority(u string) string {
	switch u {
	case "low":
		return "2"
	case "critical":
		return "5"
	}
	return "3"
}

// headerSafe strips control characters that would make a header invalid.
func headerSafe(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// Classify returns the inbox kind, level and in-app link of msg, keeping the
// ones the sender set. Kind falls back to the link's route, the sender's id
// ("chat-…" ids are chats, others sessions) or the title (budgets); Level to
// the urgency (critical → crit) or warn for failures and budgets; Link to the
// URL's "#/…" fragment or the session or chat route.
func Classify(msg Message) (kind, level, link string) {
	kind, level, link = msg.Kind, msg.Level, msg.Link
	if link == "" {
		if i := strings.Index(msg.URL, "#/"); i >= 0 {
			link = msg.URL[i:]
		}
	}
	if kind == "" {
		lt := strings.ToLower(msg.Title)
		switch {
		case strings.HasPrefix(link, "#/chat/"), strings.HasPrefix(msg.SessionID, "chat-"):
			kind = "chat"
		case strings.HasPrefix(link, "#/s/"), msg.SessionID != "":
			kind = "session"
		case strings.Contains(lt, "budget"):
			kind = "budget"
		default:
			kind = "system"
		}
	}
	if link == "" && msg.SessionID != "" {
		switch kind {
		case "chat":
			link = "#/chat/" + msg.SessionID
		case "session":
			link = "#/s/" + msg.SessionID
		}
	}
	if level == "" {
		lt := strings.ToLower(msg.Title)
		switch {
		case normUrgency(msg.Urgency) == "critical":
			level = inbox.LevelCrit
		case kind == "budget" || kind == "alert" || strings.Contains(lt, "failed") || strings.Contains(lt, "error"):
			level = inbox.LevelWarn
		default:
			level = inbox.LevelInfo
		}
	}
	return kind, inbox.NormLevel(level), link
}
