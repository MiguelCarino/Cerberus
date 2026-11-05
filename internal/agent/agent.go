// Package agent is Cerberus' built-in headless coding agent, run as
// `cerberus agent`. It drives any OpenAI-compatible model through a streaming
// tool loop (read_file, list_dir, search, run_command, write_file, edit_file)
// inside one working directory, normally a disposable git worktree owned by a
// session. Tool names, arguments and limits follow Atrium's llm_chat_engine.py
// so a model behaves the same in the desktop chat and here.
//
// The loop itself is a library (engine.go): Run drives one turn and reports
// Atrium's wire events (start, text, reasoning, tool, tool_done, ask,
// no_tools, usage, done) to a callback, with an Asker for permission cards,
// the four permission modes (ask, auto, acceptEdits, plan) and the command
// policy. The web chat (internal/chat) and the terminal agent are its two
// clients.
//
// For `cerberus agent` the terminal is the whole interface (loop.go): the
// reply streams as text, reasoning is folded into a one-line indicator, each
// tool call gets a one-line card, and approvals are asked on the PTY between
// OSC 777 markers so the session manager can mark the session as waiting.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"carino.systems/cerberus/internal/config"
	"carino.systems/cerberus/internal/files/fsafe"
	"carino.systems/cerberus/internal/platform"
)

// Exit codes.
const (
	exitOK          = 0
	exitSetup       = 1   // bad task file, bad --cwd
	exitServer      = 2   // model or server error (also flag errors, as flag does)
	exitInterrupted = 130 // SIGINT / SIGTERM
)

// options is everything one run needs, resolved from flags, env and config.
type options struct {
	Base, Model, Key, Session string
	Cwd                       string
	Origin                    string // the repo Cwd is a worktree of ($CERBERUS_REPO)
	Task                      string
	MaxSteps                  int
	Approve                   string // "ask" | "auto"
	DenyPaths                 []string
	Private                   privateArea // other sessions' records, never readable
	CommandTimeout            time.Duration
	ContextFiles              []string
	ContextMax                int
	Color                     bool
	// Resumed is the prior conversation from --resume (system prompt
	// dropped); nil for a fresh run. Task may then be empty.
	Resumed []message
	// SaveTo is where the conversation is written after each step
	// ($CERBERUS_SESSION_DIR/conversation.json), "" for nowhere.
	SaveTo string
	// Inbox is the workspace's inbox folder (files.upload_dir, slash
	// separated, "" = none); InboxSeen is the ledger of inbox files already
	// announced to the model ($CERBERUS_SESSION_DIR/inbox-seen.json, "" =
	// every inbox file is new).
	Inbox, InboxSeen string
	// Spawn calls the daemon's agent scope for the cerberus_* tools; nil
	// (no tools) unless CERBERUS_URL and a session are set and
	// agent.spawn.enabled is on.
	Spawn *SpawnClient
	// Policy is the run_command guard rail (nil = built-in rules only).
	Policy *Policy
	// Permits is the owner's saved rules, a copy the session manager keeps
	// current ($CERBERUS_AGENT_PERMITS, "" = none): they answer approvals
	// the policy does not force.
	Permits string
}

// loadConfig is a variable so tests never touch the real config file. In a
// session it reads the snapshot the session manager writes
// ($CERBERUS_AGENT_CONFIG: the agent, context and files sections, no
// token), since a sandbox hides the config folder; config.Load would then
// create a fresh default config and lose the owner's policy.
var loadConfig = func() *config.Config {
	if p := os.Getenv("CERBERUS_AGENT_CONFIG"); p != "" {
		if c, err := loadSnapshot(p); err == nil {
			return c
		}
	}
	c, err := config.Load("")
	if err != nil {
		return config.Defaults()
	}
	return c
}

// loadSnapshot reads a config snapshot over the defaults without writing
// anything.
func loadSnapshot(p string) (*config.Config, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	c := config.Defaults()
	if err := json.Unmarshal(raw, c); err != nil {
		return nil, err
	}
	return c, nil
}

// Main is `cerberus agent ARGS…`: it parses flags, runs the task and returns
// the process exit code.
func Main(args []string, stdin io.Reader, stdout io.Writer) int {
	o, code, ok := parseFlags(args, loadConfig(), stdout)
	if !ok {
		return code
	}
	// SIGHUP too: a stopped session hangs up the PTY first, and dying on it
	// by default would skip the cancellation that stops run_command's child
	// (which sits in its own process group, out of reach of the hangup).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	return run(ctx, o, stdin, stdout)
}

func parseFlags(args []string, cfg *config.Config, stdout io.Writer) (options, int, bool) {
	fs := flag.NewFlagSet("cerberus agent", flag.ContinueOnError)
	fs.SetOutput(stdout)
	fs.Usage = func() {
		fmt.Fprint(stdout, "Usage: cerberus agent [flags] [task]\n\n"+
			"The built-in coding agent: runs one task against an OpenAI-compatible\n"+
			"endpoint with file and shell tools confined to -cwd.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	base := fs.String("base", envOr("OPENAI_BASE_URL", "http://127.0.0.1:7340/v1"), "OpenAI-compatible base URL (ends in /v1)")
	model := fs.String("model", "default", "model id")
	taskFile := fs.String("task-file", "", "file holding the task (else the remaining arguments)")
	cwd := fs.String("cwd", ".", "working directory (writes are allowed only under it)")
	key := fs.String("key", "", "API key (default $OPENAI_API_KEY)")
	session := fs.String("session", os.Getenv("CERBERUS_SESSION"), "session id sent as X-Cerberus-Session")
	maxSteps := fs.Int("max-steps", orInt(cfg.Agent.MaxSteps, 60), "tool rounds before the model must answer")
	approve := fs.String("approve", orStr(cfg.Agent.Approve, "ask"), "ask | auto: ask before run_command, write_file and edit_file")
	resume := fs.String("resume", "", "conversation.json of an earlier run to continue (the task, if any, is the next user message)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return options{}, exitOK, false
		}
		return options{}, exitServer, false
	}
	fail := func(format string, a ...any) (options, int, bool) {
		fmt.Fprintf(stdout, "cerberus agent: "+format+"\n", a...)
		return options{}, exitSetup, false
	}
	if *approve != "ask" && *approve != "auto" {
		return fail("--approve must be ask or auto, not %q", *approve)
	}
	task, err := readTask(*taskFile, fs.Args(), *resume != "")
	if err != nil {
		return fail("%v", err)
	}
	var resumed []message
	if *resume != "" {
		if resumed, err = loadResume(*resume); err != nil {
			return fail("%v", err)
		}
		if len(resumed) == 0 && task == "" {
			return fail("nothing to resume: %s holds no messages and there is no new task", *resume)
		}
	}
	dir, err := filepath.Abs(*cwd)
	if err != nil {
		return fail("--cwd: %v", err)
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fail("--cwd %s is not a folder", dir)
	}
	if *key == "" {
		*key = os.Getenv("OPENAI_API_KEY")
	}
	_, noColor := os.LookupEnv("NO_COLOR")
	return options{
		Base: strings.TrimRight(*base, "/"), Model: *model, Key: *key, Session: *session,
		Cwd: dir, Origin: originRepo(dir), Task: task, MaxSteps: max(0, *maxSteps), Approve: *approve,
		DenyPaths:      append(slices.Clone(cfg.Agent.DenyPaths), configDeny(cfg)...),
		Private:        sessionData(os.Getenv("CERBERUS_SESSION"), dir),
		CommandTimeout: time.Duration(orInt(cfg.Agent.CommandTimeoutS, timeoutS)) * time.Second,
		ContextFiles:   cfg.Context.Files, ContextMax: orInt(cfg.Context.MaxBytesPerFile, 16384),
		Color:   !noColor,
		Resumed: resumed, SaveTo: conversationPath(),
		Inbox: inboxDir(cfg), InboxSeen: inboxSeenPath(),
		Spawn:   spawnClient(cfg, *key, *session),
		Policy:  NewPolicy(cfg.Agent.Policy),
		Permits: os.Getenv("CERBERUS_AGENT_PERMITS"),
	}, exitOK, true
}

// inboxDir is files.upload_dir when it is a valid relative path, else
// "inbox" (the session manager's rule, session.UploadDir).
func inboxDir(cfg *config.Config) string {
	if rel, err := fsafe.CleanRel(cfg.Files.UploadDir); err == nil && rel != "" {
		return rel
	}
	return "inbox"
}

// inboxSeenPath is $CERBERUS_SESSION_DIR/inbox-seen.json, or "" outside a
// session.
func inboxSeenPath() string {
	dir := os.Getenv("CERBERUS_SESSION_DIR")
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, inboxSeenFile)
}

// spawnClient enables the cerberus_* tools when the agent runs in a session
// (CERBERUS_URL and a session id) and agents may spawn sessions.
func spawnClient(cfg *config.Config, key, session string) *SpawnClient {
	base := os.Getenv("CERBERUS_URL")
	if base == "" || session == "" || !cfg.Agent.Spawn.Enabled {
		return nil
	}
	return NewSpawnClient(base, key, session)
}

// configDeny protects Cerberus' own config folder (the server token lives in
// config.json, API keys in secrets.json) whatever deny_paths says.
func configDeny(cfg *config.Config) []string {
	dir := ""
	if cfg.Path() != "" {
		dir = cfg.Dir()
	} else if p, err := config.DefaultPath(); err == nil {
		dir = filepath.Dir(p)
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	return []string{filepath.ToSlash(dir) + "/**"}
}

// sessionData protects Cerberus' session records in the data folder:
// conversation.json (unredacted by design, so it can hold another session's
// injected secrets), scrollback, transcript and task, and other sessions'
// worktrees and scratch folders. Only this session's own workspace (its
// worktree or scratch folder, and cwd) stays reachable. Without it a repo
// with no secrets could read another repo's through a guessed path.
func sessionData(own, cwd string) privateArea {
	data, err := platform.DataDir()
	if err != nil || !filepath.IsAbs(data) {
		return privateArea{}
	}
	sessions, worktrees := filepath.Join(data, "sessions"), filepath.Join(data, "worktrees")
	// DATA/chats holds the owner's LLM chats (tool outputs included): a
	// session never reads them either.
	a := privateArea{roots: []string{sessions, worktrees, filepath.Join(data, "chats")}, own: []string{cwd}}
	if own != "" && filepath.Base(own) == own && own != "." && own != ".." {
		a.own = append(a.own, filepath.Join(sessions, own, "workspace"), filepath.Join(worktrees, own))
	}
	return a
}

// originRepo is $CERBERUS_REPO (set by the session manager) when it is an
// absolute path and cwd is not that repo itself.
func originRepo(cwd string) string {
	r := os.Getenv("CERBERUS_REPO")
	if r == "" || !filepath.IsAbs(r) || filepath.Clean(r) == cwd {
		return ""
	}
	return filepath.Clean(r)
}

// readTask returns the task text; it may be empty only when resuming.
func readTask(file string, rest []string, resuming bool) (string, error) {
	task := strings.Join(rest, " ")
	if file != "" {
		raw, err := os.ReadFile(file)
		if err != nil {
			return "", fmt.Errorf("task file: %w", err)
		}
		task = string(raw)
	}
	if strings.TrimSpace(task) == "" && !resuming {
		return "", errors.New("no task: give --task-file F or the task as arguments")
	}
	return strings.TrimSpace(task), nil
}

// run executes one task to the end and returns the exit code. It is a thin
// client of Run: the events become the terminal transcript.
func run(ctx context.Context, o options, stdin io.Reader, stdout io.Writer) int {
	t := newTerm(stdout, o.Color)
	r := newRunner(o, t, stdin)
	head := fmt.Sprintf("cerberus agent · %s · %s · approve %s", o.Model, o.Cwd, o.Approve)
	if o.Resumed != nil {
		head += fmt.Sprintf(" · resumed (%d messages)", len(o.Resumed))
	}
	t.dimLine(head)
	r.addInbox(ctx)
	res := Run(ctx, r.runOptions(), r.handle)
	t.endLine()
	switch {
	case res.Err == nil:
		t.summary(summary(res.Done))
		t.osc("done", "")
		return exitOK
	case ctx.Err() != nil:
		t.errorLine("stopped")
		return exitInterrupted
	default:
		t.errorLine(describeError(res.Err))
		return exitServer
	}
}

func describeError(err error) string {
	var se *ServerError
	if errors.As(err, &se) && se.Status != 0 {
		return fmt.Sprintf("the server said %d: %s", se.Status, se.Message)
	}
	return err.Error()
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func orInt(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

func orStr(v, def string) string {
	if v != "" {
		return v
	}
	return def
}
