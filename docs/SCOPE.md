# Cerberus — scope

Cerberus is Carino Systems' self-hosted LLM runtime. It has three parts, which give it its name:

1. **Models.** It runs local LLMs efficiently: it loads models on demand, unloads them when idle and
   shows each request's speed and energy use.
2. **Gateway.** It serves one OpenAI-compatible endpoint that routes each request to a local model or
   to a cloud provider (API keys).
3. **Agents.** It runs coding-agent sessions (its own built-in agent, Claude Code, OpenCode and so on),
   each in its own git worktree. You can leave them running and steer them from a web UI on your
   phone or desktop.

Cerberus is a **service plus a desktop client**. The service is the Go binary `cerberus`. The desktop
base is the existing Atrium LLM chat (`llm-chat.py`), which talks to Cerberus as an OpenAI-compatible
server. The desktop client gets its own phase later.

Linux comes first. macOS and Windows are future targets, so the code must cross-compile today: no
cgo, and platform-specific code goes behind build tags, with stubs where a platform isn't supported yet.

## Principles

- **All energy goes to the models.** The control plane must sit at about 0% CPU and use less than
  30 MB RSS when idle. Pollers back off when nothing is loaded. The UI does no work while closed. There
  is no Electron.
- **Never write an inference engine.** Cerberus supervises llama.cpp (`llama-server`), LM Studio
  (`lms`), Ollama and, later, vLLM.
- **Vendor-neutral.** Everything works with no Anthropic or Claude software present. Claude Code is
  one optional agent profile, and Anthropic is one optional provider.
- **Consumer subscriptions are never proxied.** A subscription is used only by running its official
  CLI as an agent session. API keys go through the gateway.
- **The fleet's git rule.** Cerberus never creates branches. Worktrees use a detached HEAD, and
  "accept" fast-forwards `main` in the main checkout.
- **Private by default.** It binds to 127.0.0.1, with optional extra listen addresses (for example the
  Tailscale IP). A bearer token protects everything.
- **Few dependencies.** Only the Go standard library plus `github.com/creack/pty`. Vendored front-end
  assets (xterm.js) are self-hosted, never loaded from a CDN.

## Phases

| Phase | Contents |
|---|---|
| 1 (done) | Model hosting and gateway; agent sessions with worktrees, test gates, previews, a resource guard and queue, escalation and remote approval; web UI; CLI; systemd user unit |
| 2 (done) | Secrets vault; spend tracking (prices, budgets) and subscription gauges; session history (search, resume, retention); fleet tasks; voice tasks (local Whisper); HTTPS for phones; listen retry |
| Day plan (done) | Monitor; control center; file drop; safety; startup and configuration; LLM chat with mascots; extras; hardening and documentation (day phases 1 to 7, specified below) |
| Gateway phase (built, browser check pending) | Messages and Responses APIs, agent guard, request records, token-budget hygiene, stall watchdog, a stable bench (G1 to G6, specified and as built at the end) |
| 3 | GPU power-cap auto-tuner (tokens per joule), speculative-decoding and quantisation presets per GPU, vLLM backend, more than one GPU per box, several hosts (agent mode plus gateway mode) |
| 4 (client edition) | Plain chat for non-developers (Spanish first), RAG over documents, several users with roles, budgets and audit log (LFPDPPP), fully offline, Carino remote support (health alerts, updates, opt-in support tunnel) |
| 5 (desktop) | Cerberus desktop client grown from Atrium LLM chat, dynamic-island agent status, launcher "new task", bar readout of GPU power and tok/s; macOS and Windows builds |

Phase 1 runs on **one host**, but the data model already holds a host with a list of GPUs, so
several GPUs and several hosts don't need a rewrite later.

---

# Phase 1 specification (the contract between packages)

Module path: `carino.systems/cerberus`. Binary: `cerberus`. Go 1.26 or later. `CGO_ENABLED=0`. These
must all build: `GOOS=linux`, `GOOS=darwin` and `GOOS=windows` (`go build ./...`, `go vet ./...`).

## Layout and ownership

```
cmd/cerberus/        main + subcommands (serve, agent, status, task, url, install-service, version)
internal/config/     Config types, defaults, load/save, token generation       (FROZEN — see config.go)
internal/platform/   paths per OS, process-group kill, meminfo, PTY (build tags)
internal/gpu/        GPU monitor (nvidia-smi, AMD sysfs, none) + energy integrator
internal/engine/     model backends + manager (load/unload/idle TTL/make room)
internal/usage/      usage records (JSONL) + aggregates
internal/gateway/    OpenAI-compatible /v1 proxy with streaming + metrics
internal/session/    agent sessions: PTY, scrollback, worktrees, checks, preview, queue/guard, escalation, accept
internal/agent/      built-in headless coding agent (tool loop) run as `cerberus agent`
internal/notify/     notifications (command hook, ntfy, notify-send)
internal/api/        /api/* JSON + SSE, auth, preview mount, embeds web/
web/                 the web UI (embedded with go:embed via web/embed.go)
packaging/           cerberus.service (systemd user unit)
```

## Runtime paths (`platform`)

- Config: `os.UserConfigDir()/cerberus/config.json` (on Linux, `~/.config/cerberus/config.json`). It's
  created with defaults and a random token on first run, mode 0600.
- Data: Linux `$XDG_DATA_HOME` or `~/.local/share/cerberus`; macOS `~/Library/Application Support/Cerberus`;
  Windows `%LOCALAPPDATA%\Cerberus`. Inside it:
  - `sessions/<id>/meta.json` and `sessions/<id>/scrollback.log` (capped, `ScrollbackMiB`)
  - `worktrees/<id>/` (the detached worktree)
  - `usage.jsonl`
  - `logs/<model-id>.log` (backend process output)

## GPU (`internal/gpu`)

```go
type GPU struct { Index int; Name string; Vendor string; MemTotalMiB, MemUsedMiB int; PowerW, PowerLimitW float64; UtilPct int; TempC int; PState string }
type Snapshot struct { Time time.Time; GPUs []GPU; EnergyJ float64 /* cumulative since start, all GPUs */ }
type Monitor struct{ ... }
func New(cfg config.GPUConfig, log *slog.Logger) *Monitor
func (m *Monitor) Run(ctx context.Context)          // poll loop
func (m *Monitor) Snapshot() Snapshot                // idle: re-queried on demand when older than IdleIntervalMs
func (m *Monitor) SetActive(active bool)             // poll every ActiveIntervalMs when true; no polling when false
func (m *Monitor) Refresh(ctx context.Context)       // query now (before make-room decisions)
func (m *Monitor) EnergyJ() float64                  // cumulative joules (trapezoid), extrapolated from the last sample
func (m *Monitor) FreeVRAMMiB() int                  // sum free; -1 if unknown (idle: on demand like Snapshot)
```

Backends: NVIDIA via `nvidia-smi --query-gpu=index,name,memory.total,memory.used,power.draw,power.limit,utilization.gpu,temperature.gpu,pstate --format=csv,noheader,nounits`; AMD via
`/sys/class/drm/card*/device/{mem_info_vram_total,mem_info_vram_used,gpu_busy_percent,hwmon/hwmon*/power1_average}`; otherwise none.
Run one query per tick, never a resident `-lms` process. A missing tool is not an error: the result is an empty snapshot.

## Engine (`internal/engine`)

```go
type ModelInfo struct { ID, Backend, Name string; Local, Loaded bool; SizeMiB int; Ctx int; LastUsed time.Time; InFlight int; Vision bool }
type Endpoint struct { BaseURL string /* ends in /v1 */; Model string /* id the backend expects */; Headers map[string]string; Local bool; Backend string }
type Backend interface {
    Name() string                                              // "llamacpp" | "lmstudio" | "ollama" | "openai"
    Available() bool
    List(ctx context.Context) ([]ModelInfo, error)
    Load(ctx context.Context, m ModelInfo) (Endpoint, error)   // idempotent
    Unload(ctx context.Context, m ModelInfo) error
    Endpoint(m ModelInfo) Endpoint
}
type Manager struct{ ... }
func NewManager(cfg *config.Config, gpu *gpu.Monitor, log *slog.Logger) *Manager
func (m *Manager) Run(ctx context.Context)                                  // idle reaper (per-model TTL), refresh lists, stops spawned processes on ctx done
func (m *Manager) Models(ctx context.Context) []ModelInfo                   // cached ≤5 s
func (m *Manager) Acquire(ctx context.Context, id string) (Endpoint, func(), error) // load if needed (making room), InFlight++; release func: InFlight--, LastUsed=now
func (m *Manager) Load(ctx context.Context, id string) error
func (m *Manager) Unload(ctx context.Context, id string) error
func (m *Manager) Shutdown(ctx context.Context)
```

- **llamacpp:** configured `ModelConfig` entries with `Path`. Spawns `cfg.Engines.LlamaCpp.Binary -m PATH --host 127.0.0.1 --port <free> -c CTX -np PARALLEL --jinja --flash-attn on --metrics` plus `Args`, then waits for `GET /health` to return 200 (timeout `LoadTimeoutSec`). Process output goes to `logs/<id>.log`. Unload sends SIGTERM to the process group, then SIGKILL after 10 s. Also scans `ModelDirs` for `*.gguf` (id = file stem, mmproj files skipped).
- **lmstudio:** `lms ls --json`, `lms ps --json`, `lms load <key> --context-length N --ttl S -y --identifier <key>`, `lms unload <key>`, `lms server start` if `GET {base}/models` fails. Base from config (default `http://127.0.0.1:1234/v1`).
- **ollama:** `GET /api/tags`, `GET /api/ps`; load = `POST /api/generate {"model":M,"keep_alive":"<ttl>"}`; unload = `keep_alive:0`; OpenAI base `{url}/v1`.
- **openai (cloud providers):** one backend per `ProviderConfig`. Model IDs are `<provider>/<model>`. Models come from config or from `GET {base}/models`, cached for 10 min. The key comes from `KeyEnv`, or else from `secrets.json` (in the config dir, mode 0600, `{"<provider>":"key"}`). Load and Unload do nothing.
- **Making room** (local only): before loading, if `gpu.FreeVRAMMiB()` is known and below `SizeMiB*1.15 + 512`, unload idle local models (InFlight==0, LRU first) until it fits. If it still does not fit, load anyway: the engines keep the rest in system RAM (slower, but it works; MoE models do this well). Refuse with `ErrNoRoom` only when free VRAM + free RAM cannot hold it, unless `Engines.AllowOverCommit`.
- **Idle reaper:** a local model with InFlight==0 and `now-LastUsed > TTL` gets unloaded. The TTL comes from the model, else `Engines.IdleTTLSec`. It calls `gpu.SetActive(anyLoaded || anyInFlight)`.
- ID resolution: an exact match first, then a unique suffix or prefix, then the alias `default` → `cfg.Engines.DefaultModel`.

## Usage (`internal/usage`)

```go
type Record struct { Time time.Time; Model, Backend, Client string; Local bool; PromptTokens, CompletionTokens int; TTFTMs, DurationMs int64; TokPerSec float64; EnergyJ float64; TokPerJ float64; Status int; Error string; SessionID string }
type Store struct{ ... }
func Open(path string, keep int) (*Store, error)   // appends JSONL; keeps last `keep` in memory
func (s *Store) Add(r Record)
func (s *Store) Recent(n int) []Record
type Agg struct { Model string; Requests, PromptTokens, CompletionTokens int; EnergyJ, AvgTokPerSec, AvgTokPerJ float64 }
func (s *Store) Aggregate(since time.Time) []Agg
func (s *Store) Close() error
```

## Gateway (`internal/gateway`)

```go
func New(eng *engine.Manager, mon *gpu.Monitor, store *usage.Store, log *slog.Logger) *Gateway
func (g *Gateway) Handler() http.Handler   // mounted at /v1 by api (paths below are relative to /v1)
```

- `GET /models` returns the OpenAI list format with every `engine.Models`, including `owned_by: backend`.
- `POST /chat/completions`, `POST /completions`, `POST /embeddings`: read the body, `Acquire(model)`,
  rewrite `model` to `Endpoint.Model`, and for streaming requests set `stream_options.include_usage=true`.
  Then proxy to `Endpoint.BaseURL + path` with its headers, flushing every SSE line as it arrives, and
  propagate client cancellation upstream. It measures TTFT (time to the first content byte) and token
  counts (from `usage`, or else the number of content deltas). Energy is `gpu.EnergyJ()` delta ÷
  concurrent local in-flight requests, local models only. It writes a `usage.Record`, with `Client`
  taken from the `X-Cerberus-Client` header and `SessionID` from `X-Cerberus-Session`.
- Errors use the OpenAI error JSON `{"error":{"message":..,"type":..}}`. An unknown model returns 404.

## Sessions (`internal/session`)

States: `queued → starting → running ⇄ waiting → checking → passed | failed → (retrying | escalated) …`, with terminal states `accepted | discarded | stopped | error`.
`waiting` means the agent asked for input (the OSC marker below), or produced no output for `IdleWaitSec` while still running (interactive CLIs).
When the process exits, checks run if the repo has any. With no checks configured, exit code 0 → `passed`.

**Workspaces.** Every session works in one of three places (`Info.Workspace`):

| Workspace | Chosen by | Agent works in | Diff / Accept | Discard |
|---|---|---|---|---|
| `repo` | a folder that is a git repo top level (or `Repo`) | a detached worktree `DATA/worktrees/<id>` | yes | removes the worktree |
| `folder` | any other folder | the folder itself, in place (changes are live) | no | forgets the session; **never** touches the folder |
| `scratch` | nothing chosen ("decide later") | a fresh empty `DATA/sessions/<id>/workspace` | no; **Save to folder** / **Download zip** at any time | deletes the scratch folder |

- Resolution: `Workspace:"scratch"` → scratch. `Repo` set (older clients) or `Workspace:"repo"` → repo.
  `Dir` set (and `Workspace` empty or `"folder"`) → repo if `Dir` is a git repo top level, otherwise folder.
  Nothing set → scratch.
- Allowed folders: existing folders under the home folder (symlinks resolved), plus `cfg.Repos` paths and
  `RepoRoots`. Refused: `deny_paths` matches, Cerberus' own data and config folders (and folders containing
  them), and the home folder itself for folder sessions. RepoRoots still feed the "detected repos" list.
- Folder sessions take checks and preview only from `<folder>/.cerberus.json`. Scratch sessions likewise from
  their workspace.
- Sessions saved before workspaces existed load as `repo` sessions.

```go
type TaskSpec struct {
    Workspace string // "repo" | "folder" | "scratch"; "" = decided from Repo/Dir (nothing = scratch)
    Dir    string   // absolute folder; a git repo top level becomes a repo session
    Repo   string   // absolute path of a git repo (older clients; same as Workspace "repo" + Dir)
    Task   string   // the instruction
    Agent  string   // AgentProfile name (default cfg.DefaultAgent)
    Model  string   // model id for agents that take one (default engine default)
    EscalateTo string // profile to hand over to after MaxAttempts failed check runs ("" = none)
    MaxAttempts int // attempts with Agent before escalating (default cfg.Sessions.MaxAttempts)
    Count  int      // fan-out: N independent sessions with the same task (default 1), each its own worktree
    Title  string
}
type Info struct {
    ID, Title, Repo, Task, Agent, Model, EscalateTo string
    Workspace, Dir string // where the session works (Dir "" for scratch)
    State string; Attempt int; ExitCode *int; Created, Updated time.Time
    Worktree /* the agent's working directory in every workspace */, BaseCommit /* repo only */ string; ChecksOutput string; PreviewURL string; Error string; Group string /* fan-out group id */
    WaitingReason string; Cols, Rows int
}
type Event struct { Type string /* "state" | "created" | "removed" */; Session Info }
type Manager struct{ ... }
func NewManager(cfg *config.Config, eng *engine.Manager, gpu *gpu.Monitor, notifier *notify.Notifier, log *slog.Logger) (*Manager, error) // loads existing sessions from disk (live ones become "stopped", reason "daemon restarted"; worktree kept)
func (m *Manager) Run(ctx context.Context)                       // queue scheduler (every 2 s + on signal)
func (m *Manager) Create(ctx context.Context, t TaskSpec) ([]Info, error)
func (m *Manager) List() []Info
func (m *Manager) Get(id string) (Info, bool)
func (m *Manager) Input(id string, data []byte) error
func (m *Manager) Resize(id string, cols, rows int) error
func (m *Manager) Attach(id string) (backlog []byte, ch <-chan []byte, detach func(), err error) // backlog = last ScrollbackTailKiB of output
func (m *Manager) FollowUp(ctx context.Context, id, text string) error  // running → typed into the PTY + "\r"; finished → new attempt of the same agent in the same worktree with text as the task
func (m *Manager) RunChecks(ctx context.Context, id string) error
func (m *Manager) Stop(id string) error
func (m *Manager) Diff(id string) (string, error)                      // unified diff of worktree vs BaseCommit, including untracked files
func (m *Manager) Accept(ctx context.Context, id string, o AcceptOpts) (string /*commit sha*/, error)
func (m *Manager) Discard(id string) error                             // kill, remove what Cerberus owns (worktree / scratch folder; never a user folder), state discarded
func (m *Manager) SaveTo(id, dir string, overwrite bool) (int, error)  // scratch only: copy the workspace into dir (created if missing); existing files refused unless overwrite
func (m *Manager) Archive(id string, w io.Writer) error                // scratch only: the workspace as a zip
func (m *Manager) Browse(dir string, hidden bool) (Listing, error)     // folder picker: sub-folders of dir (default home), git repos marked
func (m *Manager) Subscribe() (<-chan Event, func())
func (m *Manager) Shutdown(ctx context.Context)
type AcceptOpts struct { Message string; Push bool }
```

- **Worktree:** `git -C REPO worktree add --detach DATA/worktrees/<id> <HEAD of main>`. No branch is ever created. BaseCommit is recorded.
- **Accept:** in the worktree, `git add -A`; if there are changes, commit, detached, with the identity from
  `cfg.Git` (falling back to the repo's git config). The message is `o.Message`, or else the session title.
  There are never attribution trailers. Then rebase the worktree's commits onto the repo's current `main`
  (detached `git rebase`), squash them to a single commit (`git reset --soft <main>` + commit), and in the
  main checkout run `git merge --ff-only <sha>`. The main checkout must be on `main`, otherwise refuse
  with a clear message. Git itself protects uncommitted conflicting changes, and that error is surfaced.
  Then `git push origin main` if `o.Push`. Then remove the worktree and set the state to `accepted`.
  On any failure, nothing is lost: the worktree stays and the error is returned.
- **PTY:** `platform.StartPTY(cmd, cols, rows)`. The child runs in its own process group with environment
  `CERBERUS_SESSION=<id>`, `CERBERUS_URL`, `OPENAI_BASE_URL=http://127.0.0.1:<port>/v1`,
  `CERBERUS_REPO=<repo>`, `OPENAI_API_KEY=<cfg.AgentToken()>` (valid for `/v1` only, never the admin
  token), `TERM=xterm-256color`. When the agent exits, or 10 s after a stop, every process left in its
  session is killed. When `Guard.SystemdSlice` is set and `systemd-run` is on PATH
  (Linux), the command is wrapped as `systemd-run --user --scope --quiet --slice=<slice> -p MemoryMax=<Guard.SessionMemoryMax> -- cmd…`.
- **Agent profiles** (`config.AgentProfile`): `Command []string` with placeholders `{task}`, `{task_file}`,
  `{model}`, `{base_url}`, `{cwd}`, `{session}`. The task is also written to `sessions/<id>/task.md`. Built-in
  defaults (see config defaults): `cerberus` (the built-in agent), `claude` (interactive), `claude-headless`,
  `opencode`, `aider`, `shell`. A profile whose binary isn't on PATH is listed as unavailable.
- **Context:** for the `cerberus` agent, the agent itself loads context (see below). For other profiles,
  nothing is injected, since they read CLAUDE.md or AGENTS.md natively.
- **Markers:** an agent may print OSC `ESC ] 777 ; cerberus ; <kind> [; text] BEL` with kind `waiting`
  (text = reason), `resumed` or `done`. The session manager strips these from the stream and changes state.
- **Checks:** taken from `RepoConfig.Checks` (each a shell command run with `bash -c` in the worktree,
  `ChecksTimeoutSec`), or, if the repo has no config entry, from `<worktree>/.cerberus.json` `{"checks":[...],"preview":{...}}`.
  The combined output is clipped to 64 KiB and goes into `ChecksOutput`.
- **Retry and escalate:** checks fail and `Attempt < MaxAttempts` → a new attempt of the same agent in the
  same worktree, with the task "The checks failed. Fix the problem. Original task: … Check output: …".
  Checks fail and attempts are exhausted with `EscalateTo` set → a new attempt with the EscalateTo profile
  (state `escalated` → `running`), with the original task + the check output + "a previous agent left work
  in this tree; review it".
- **Preview:** `RepoConfig.Preview` or `.cerberus.json` preview `{"dir":"."}` → served by api at
  `/preview/<id>/` as static files from the worktree subdir (index.html, no directory listing beyond a
  simple list). `PreviewURL` = `/preview/<id>/`, which redirects (authenticated) to
  `/preview/<id>/~<cap>/` where `cap` is a per-session capability; preview pages are served with
  `Content-Security-Policy: sandbox allow-scripts …` (opaque origin, no access to `/api`).
- **Guard and queue:** a session starts only if running sessions < `Guard.MaxSessions`, `MemAvailable` ≥
  `Guard.MinFreeMiB` (platform.MemAvailableMiB; unknown → allowed) and, for a local model, free VRAM is
  unknown or ≥ the model's estimate *or* the model is already loaded. Otherwise it stays `queued` with
  `WaitingReason` explaining why.
- **Notify:** on entering `waiting`, `passed`, `failed`, `escalated` or `error`, call `notifier.Send(...)`.
- **Persistence:** meta.json is rewritten on every state change. Scrollback is appended to scrollback.log
  and truncated to its last half when it exceeds the cap.

## Built-in agent (`internal/agent`, run as `cerberus agent`)

```
cerberus agent --base URL --model M --task-file F --cwd DIR [--key K] [--session ID] [--max-steps N] [--approve auto|ask]
```

- An OpenAI-compatible streaming chat loop with tools `read_file`, `list_dir`, `search`, `run_command`,
  `write_file` and `edit_file`, matching the semantics of Atrium's `llm_chat_engine.py` (clipping limits,
  exact-once edit). If the server rejects tools, it falls back to plain chat.
- The system prompt contains the working directory, the tools, and the contents of `AGENTS.md` and
  `CLAUDE.md` from the repo root (if present). It also includes each file listed in
  `cfg.Context.Files` (for example the memory index), each clipped to `cfg.Context.MaxBytesPerFile`.
- Output is human-readable for the terminal: the streamed reply text, then a one-line card per tool
  call (`▸ run_command: npm test`) with a clipped result.
- **Approval:** `--approve ask` (the default from config `Agent.Approve`) asks before `run_command`,
  `write_file` and `edit_file`. The agent prints the request, emits `OSC 777;cerberus;waiting;approval`,
  then reads a line from stdin: `y` runs it, `a` runs it and allows that tool for the rest of the
  session, `n` refuses it (the model is told it was refused), and any other text refuses it and is sent
  to the model as the reason. After the answer it emits `resumed`. Paths matching `cfg.Agent.DenyPaths`
  globs are always refused, and paths outside `--cwd` are refused for writes.
- At the end it prints a summary line, emits `done` and exits 0. It exits 2 on model or server errors.

## Notify (`internal/notify`)

```go
type Message struct { Title, Body, Urgency string /* low|normal|critical */; SessionID string; URL string }
func New(cfg config.NotifyConfig, log *slog.Logger) *Notifier
func (n *Notifier) Send(msg Message)    // non-blocking; never fails the caller
```

Channels, all optional: `Command` (argv, with `{title}`, `{body}` and `{urgency}` substituted; the default
is Atrium's `~/.config/hypr/scripts/notify` when it exists), `NtfyURL` (a POST with a Title header), and
`notify-send` as a fallback when `Command` is empty and the binary exists. Sends are rate-limited per
session (at most one every 10 s per state).

## API (`internal/api`)

```go
type Deps struct { Cfg *config.Config; Engine *engine.Manager; GPU *gpu.Monitor; Usage *usage.Store; Gateway *gateway.Gateway; Sessions *session.Manager; Log *slog.Logger; Version string }
func New(d Deps) http.Handler
```

- **Auth:** `Authorization: Bearer <token>`, a cookie `cerberus_token`, or `?token=` on GET (which sets the
  cookie and redirects without it). Comparisons are constant-time. `/v1/*` from a loopback address needs
  no auth when `cfg.Server.LocalNoAuth` is set, so desktop clients work with no key, unless it looks like
  a web page sent it (an `Origin` header, a non-local `Host`, or a write without a JSON `Content-Type`).
  `cfg.AgentToken()` works for `/v1/*` only. Cookie-authenticated `/api/*` requests from another site or
  a preview page are refused. Everything else always needs auth, except `/login`, `/assets/*`,
  `/healthz` and preview capability URLs.
- Endpoints (JSON):
  - `GET /healthz` → `{"ok":true}`
  - `GET /api/status` → `{version, host, uptime, gpu: Snapshot, models: [ModelInfo], sessions: {counts}, guard: {mem_available_mib, max_sessions, running}, agents: [{name, available}], repos: [...]}`
  - `GET /api/models`, `POST /api/models/{id}/load`, `POST /api/models/{id}/unload` (the id is path-escaped and may contain `/`)
  - `GET /api/usage?limit=100` → `{recent: [Record], agg_24h: [Agg]}` (agg_24h from the quarter-hour totals, not the recent ring)
  - `GET /api/repos` → configured repos plus git repos found one level under each `RepoRoots` entry: `[{name, path, checks: bool, preview: bool}]`
  - `GET /api/sessions`, `POST /api/sessions` (TaskSpec JSON) → `[Info]`
  - `GET /api/fs?path=ABS[&hidden=1]` → `{path, parent, git, home, entries: [{name, path, git}]}`: sub-folders only,
    sorted, dot-folders only with `hidden=1`, denied and Cerberus-owned folders left out, nothing outside the home
    folder (default `path` = home; `parent` is `""` at home)
  - `GET /api/sessions/{id}`, `GET /api/sessions/{id}/diff` → `text/plain` (repo sessions only)
  - `GET /api/sessions/{id}/archive` → `application/zip` of a scratch session's workspace
  - `GET /api/sessions/{id}/stream` → SSE: first `event: backlog` then `event: out`, data = base64 bytes; a `: ping` every 20 s
  - `POST /api/sessions/{id}/input {"data":"..."}`, `/resize {"cols":..,"rows":..}`, `/followup {"text":..}`,
    `/checks`, `/stop`, `/accept {"message":..,"push":bool}` (repo only), `/discard`,
    `/save {"dir":ABS,"overwrite":bool}` (scratch only) → `{ok, files}`
  - `GET /api/events` → SSE of session `Event`s (`event: session`)
- `/v1/` → gateway. `/preview/<id>/…` → static files of the session's preview dir.
- `/` → the embedded web UI (`web.FS`), with `Cache-Control: no-cache` for index.html.

## Web UI (`web/`)

- A single-page app in vanilla JS (ES modules, no build step), embedded via `web/embed.go`
  (`//go:embed all:dist` or the file list) and exposing `var FS embed.FS`.
- It follows the Carino Branding look (gold on black, tokens from `~/Github/Branding/carino-ui.css`, copied
  into `web/` together with the needed self-hosted fonts). The top header follows the fleet navbar
  convention: the brand "Carino" + app tag "Cerberus", with no footer.
- Mobile first (breakpoints 640 and 900 px), usable one-handed. Views:
  - **Sessions:** the list grouped by state, with a badge when waiting; the "New task" button
    opens a form: workspace first ("None (decide later)" preselected, or "Folder": detected repos as
    quick picks, then a folder browser over `/api/fs`, with a note that a git repo gets an isolated
    worktree and a plain folder is edited live), then task, agent, model, escalate-to, count.
    A scratch session shows "Save to folder…" (the same browser) and "Download zip"; a folder session
    shows its path and has no Diff or Accept.
  - **Session:** a live terminal (xterm.js plus the fit addon, vendored in `web/vendor/`), an input bar
    with quick keys (Enter, y, n, a, Esc, Ctrl-C, ↑, ↓, Tab), state and attempt, and buttons for Run
    checks, Stop, Diff, Preview, Accept (message and push toggle) and Discard. The diff view lets you
    tap a line to comment, and "Send comments" posts them as one follow-up.
  - **Models:** GPU cards (power, VRAM, temperature, utilisation), models with load and unload, and
    per-model tok/s and tok/J (24 h).
  - **Usage:** recent requests.
  - **Login:** paste the token (stored as a cookie by the server via `?token=`).
- No polling while the tab is hidden (Page Visibility API). SSE for events and the open session only.

## CLI (`cmd/cerberus`)

- `cerberus serve`: load config, start gpu, engine, sessions, usage and api; serve on `Server.Listen`
  and each `Server.ExtraListen`; on SIGTERM, shut down gracefully (stop sessions, unload spawned models).
- `cerberus agent …`: the built-in agent (above).
- `cerberus status`: a short human summary, read from `GET /api/status`.
- `cerberus task [--dir D | --repo R | --scratch] [--agent A] [--model M] [--escalate P] [--count N] "task…"`: `POST /api/sessions`.
  No flag: the git repo containing the current folder (repo session), else scratch. `--dir`: that folder
  (the daemon makes a git repo top level a repo session, anything else a folder session).
- `cerberus url`: prints the UI URL with `?token=` for every listen address (for pairing a phone).
- `cerberus install-service`: writes `~/.config/systemd/user/cerberus.service` from `packaging/`
  (embedded), then `systemctl --user daemon-reload && systemctl --user enable --now cerberus` (Linux only;
  elsewhere it prints "not supported yet").
- `cerberus version`.

## Desktop integration (Phase 1 scope)

- Atrium LLM chat can use Cerberus with no code change: set `server_url` to `http://127.0.0.1:7340/v1` in
  llm.json (`LocalNoAuth` covers the key).
- Notifications go through Atrium's `scripts/notify` when present.
- `docs/DESKTOP.md` describes this and the Phase 5 plan.

## Acceptance (Phase 1)

- `go build ./...` and `go vet ./...` pass for linux, darwin and windows; `go test -race ./...` passes on Linux.
- An end-to-end test (fake OpenAI server + fake agent command in a temp git repo): create a session, see
  the output over Attach, the waiting marker, input, exit, checks pass, accept fast-forwards main, and no
  branch exists afterwards (`git branch --list` shows only main).
- Gateway test: a streaming proxy against a fake server records TTFT, tokens and usage; cancellation propagates.
- Idle RSS of `cerberus serve` with no models under 30 MB; idle CPU about 0%.

---

# Phase 2 specification

Phase 2 adds: a **secrets vault**, **spend and subscription tracking** (prices, budgets, Claude Code plan
gauge), **session history** (search, resume, retention), **fleet tasks** (one instruction → one session per
repo), **voice tasks** (local Whisper) and two Phase 1 leftovers: **HTTPS** for phone microphones and
**retrying extra listen addresses** (Tailscale up after boot). Config types are in `internal/config/phase2.go`
(plus fields added to Server, Provider, AgentProfile, Repo and Sessions configs) — frozen like Phase 1:
add only.

## Vault (`internal/vault`, new)

```go
type Vault struct{ ... }
func Open(cfg *config.Config, log *slog.Logger) (*Vault, error)
func (v *Vault) Backend() string                         // "secret-service" | "file"
func (v *Vault) List() ([]Item, error)                   // names + metadata, never values
func (v *Vault) Get(name string) (string, error)         // ErrNotFound
func (v *Vault) Set(name, value, note string) error
func (v *Vault) Delete(name string) error
func (v *Vault) Values() []string                        // all current values (for redaction), cached, refreshed on Set/Delete
type Item struct { Name, Note string; Updated time.Time; Length int }
```

- **secret-service:** `secret-tool store --label="Cerberus: NAME" app cerberus name NAME`, with the value
  written on **stdin**, never in argv. Look-up is `secret-tool lookup app cerberus name NAME`, listing is
  `secret-tool search --all app cerberus` (parse the attributes; no secrets are printed for listing beyond
  what search outputs — discard any `secret =` lines), and clear is `secret-tool clear`. Metadata (notes,
  updated, length) goes in `vault-index.json` in the config dir (0600, no values). "auto" probes the
  backend once with a harmless search; a failure or a locked or absent provider falls back to file.
- **file:** `vault.json` in the config dir, AES-256-GCM per value, with the key in `vault.key` (32 random
  bytes, 0600). This protects against accidental leaks (backups, grep, screenshots of config). It is not
  protection against a local attacker, and the docs say so.
- **Migration:** on Open, entries from the old plaintext `secrets.json` are imported, and the file is
  renamed `secrets.json.migrated` with mode 0600 kept.
- `config.Secret(name)` stays as-is for compatibility. **engine** gets a provider's key from
  `KeyEnv` → vault(`KeySecret` or `Name`) → `cfg.Secret`. The engine takes an optional key-lookup function:
  `engine.WithSecrets(func(name string) string)`, a variadic option on NewManager.
- Names: `^[A-Za-z0-9_.-]{1,64}$`. Values ≤ 16 KiB.

## Spend and subscriptions (`internal/usage` extended, new `internal/subs`)

- usage.Record gains `CostUSD float64` and `Provider string`. The gateway fills both: the price comes from
  `cfg.Prices[model id]`, else `cfg.Prices["<provider>/*"]`, else 0 for local models (CostUSD stays 0;
  energy is the local cost). Cached input tokens are used when upstream reports
  `usage.prompt_tokens_details.cached_tokens`.
- `usage.Store.Spend(provider string, since time.Time) float64` and `Store.SpendByProvider(since) map[string]float64` come from the quarter-hour buckets (keep 62 days of totals for months; 15 min so local midnight in +5:30/+5:45 zones falls on a bucket boundary, and Open reads the file back from the start of the 62-day window, not just its tail).
- **Budgets (gateway):** before proxying a request to a cloud model, check every matching Budget for the
  current period (day = local midnight, month = 1st). Spend ≥ HardUSD → `429`, with an OpenAI error
  `{"error":{"type":"budget_exceeded","message":"<provider> <period> budget of $X reached"}}`. Crossing
  SoftUSD (once per period and provider) → `notify` (inject a `func(notify.Message)` into gateway via
  an option `gateway.WithNotifier`).
- **subs:** `type Tracker` with `New(cfg, log)`, `Run(ctx)` and `Snapshot() []PlanUsage`.
  `PlanUsage{Name, Kind string; Window5hTokens, WeeklyTokens int; Window5hLimit, WeeklyLimit int; Window5hResetAt time.Time; LastActivity time.Time; Sessions int; Err string}`.
  For kind `claude-code`, it reads `*.jsonl` under DataDir (recursive, one level of project folders).
  It parses each `assistant` line's `message.usage` (input_tokens, output_tokens, cache_creation_input_tokens,
  cache_read_input_tokens) with `timestamp`, and de-duplicates by `message.id` (the CLI writes a message more
  than once). With AllSessions=false, only project folders whose decoded path is inside the Cerberus
  worktrees or sessions dir count. Tokens = input + output + cache_creation (cache reads are listed
  separately as `CacheReadTokens` and not counted toward limits). The 5 h window starts at the first
  message after a 5 h gap (Claude's own block rule); reset = start + 5 h.
- **Energy and CPU:** subs is incremental. It remembers the file size and offset per file, rescans only
  on change (stat on a 60 s ticker), and runs only while the UI is subscribed or a Claude session is live
  (`Tracker.SetActive(bool)`), with otherwise one scan every 15 min.

## Session history, resume, retention (`internal/session`)

- **Text index:** alongside scrollback.log, the pump writes `transcript.txt`, which is ANSI- and
  control-stripped (CSI, OSC, other escapes; `\r` handling: keep the last write of a line), capped at
  4 MiB (truncate to last half).
- `func (m *Manager) Search(q SearchQuery) []SearchHit` with `SearchQuery{Text, Repo, Agent, State string; Since, Until time.Time; Limit int}` and `SearchHit{Session Info; Snippets []string /* ≤3, ±80 chars around matches */; Score int}`.
  Matching is case-insensitive, all words must appear in title + task + transcript, and title or task
  hits are weighted higher. It scans files on demand, which is enough for thousands of sessions (stop at
  Limit, default 50).
- **Resume:** `func (m *Manager) Resume(ctx context.Context, id string, text string) error` works for a
  session in a final non-terminal state (passed, failed, stopped, error) whose workspace still exists. It
  starts a new attempt in the same workspace with the profile's ResumeCommand when set, else the
  profile's Command with task = text, or "Continue where you left off." when text is empty. Accepted or
  discarded sessions are refused. `{resume_file}` = `sessions/<id>/conversation.json`.
- **Built-in agent:** `--resume FILE` loads the prior messages (system prompt rebuilt fresh) and, if the
  task file has text, appends it as the next user message. The agent always writes its conversation to
  `$CERBERUS_SESSION_DIR/conversation.json` after each step (atomic), if that variable is set. Sessions
  set `CERBERUS_SESSION_DIR`.
- **Retention:** a daily prune (and on start) deletes session dirs in a final state (accepted, discarded,
  passed, failed, stopped, error) older than RetentionDays. It never deletes a repo worktree with
  unaccepted changes (`git status --porcelain` non-empty, or commits ahead of BaseCommit): those are
  kept and listed. It never touches folder-mode folders. `func (m *Manager) Prune(ctx) (PruneReport, error)`.
- **Secrets into sessions:** the env for a session = profile.SecretEnv ∪ repo.SecretEnv, resolved through
  the vault at launch. A missing secret → state error naming it. The output pump **redacts** every vault
  value of length ≥ 6 (`vault.Values()`) from scrollback, transcript and live output as `••••••`, handling
  values split across reads with a carry of max-value-length − 1 bytes. The session manager gets the vault
  via `session.WithVault(*vault.Vault)` (option on NewManager).

## Fleet tasks (`internal/session`)

```go
type BatchSpec struct { Repos []string; Task string /* may use {repo} {name} */; Agent, Model, EscalateTo string; MaxAttempts int; Title string }
type BatchInfo struct { ID, Title, Task, Agent, Model string; Created time.Time; Sessions []string; Counts map[string]int }
func (m *Manager) CreateBatch(ctx context.Context, b BatchSpec) (BatchInfo, []Info, error)
func (m *Manager) Batches() []BatchInfo
func (m *Manager) Batch(id string) (BatchInfo, []Info, bool)
func (m *Manager) AcceptBatch(ctx context.Context, id string, o AcceptOpts) []BatchResult  // accepts every session in state passed; others skipped with reason
func (m *Manager) DiscardBatch(id string) []BatchResult
type BatchResult struct { SessionID, Repo, Result /* accepted|skipped|failed|discarded */, Detail, Commit string }
```

- Every repo must be a repo-mode workspace (validated like Create). All sessions are queued at once and the
  guard admits them as capacity allows. Batch metadata lives in `DATA/batches/<id>.json`, and Info.Group = batch id.
- Repo selection helper: `func (m *Manager) FindRepos(sel RepoSelector) []RepoMatch` with
  `RepoSelector{Roots []string /* default RepoRoots */; HasFile string /* glob relative to repo root, e.g. "carino-navbar.js" */; NameGlob string}`
  and `RepoMatch{Name, Path string; Dirty bool}`. Dirty repos are flagged (accept may fail later).

## Voice (`internal/voice`, new)

```go
type Transcriber struct{ ... }
func New(cfg config.VoiceConfig, log *slog.Logger) *Transcriber
func (t *Transcriber) Available() (bool, string)                 // backend name or reason
func (t *Transcriber) Transcribe(ctx context.Context, audio io.Reader, mime string) (string, error)
```

- Input: browser MediaRecorder output (webm/opus, ogg/opus, mp4/aac) or wav, at most MaxSeconds and 25 MiB.
  It's converted with `ffmpeg -nostdin -i pipe:0 -ac 1 -ar 16000 -f wav <tmp>.wav` (in a temp dir, 0700,
  removed after).
- Backends: **auto** tries `~/.config/hypr/scripts/voice.py transcribe {wav} --json` (Atrium, runs
  faster-whisper locally), then `whisper-cli -m <first ggml model in ~/models or ~/.local/share/whisper> -f {wav} -nt -np`,
  then `openai` if URL is set. **command** = Command argv. **openai** = multipart POST to
  `{URL}/audio/transcriptions` (model, language, file). Output: JSON `{"text"}` or plain text, trimmed.
  One transcription at a time (mutex), 120 s timeout.
- Nothing leaves the machine unless the user configures a non-local URL.

## Serve: HTTPS and listen retry (`cmd/cerberus`)

- With TLSCert and TLSKey set, every **ExtraListen** address serves HTTPS (`tls.LoadX509KeyPair`, reloaded on
  SIGHUP and on file mtime change checked hourly). The primary loopback Listen stays HTTP, so desktop
  clients are unchanged. Phones need HTTPS for the microphone (`getUserMedia` requires a secure context).
  The README documents `tailscale cert <name>.ts.net`, plus `public_url`.
- An ExtraListen address that fails to bind is retried with backoff (5 s → 60 s, forever) until it binds,
  logging once per state change. This fixes the Tailscale-not-up-at-boot gap.
- The auth cookie gets `Secure` when served over TLS.

## API additions (`internal/api`)

- Vault: `GET /api/secrets` → `{backend, items:[Item]}`; `PUT /api/secrets/{name}` `{"value","note"}`;
  `DELETE /api/secrets/{name}`. Values are never returned. Writes need the CSRF header as usual.
- Spend: `GET /api/usage` gains `spend: {today: {provider: usd}, month: {...}}`, `budgets: [{Budget fields + spent, state: ok|soft|hard}]`,
  `plans: [PlanUsage]`. Opening the Usage view calls `POST /api/usage/watch` (marks subs active for 10 min).
- History: `GET /api/search?q=&repo=&agent=&state=&since=&until=&limit=` → `[SearchHit]`;
  `POST /api/sessions/{id}/resume {"text"}`; `POST /api/prune` → PruneReport; `GET /api/sessions/{id}/transcript` → text/plain.
- Fleet: `GET /api/repos/find?has=&name=` → `[RepoMatch]`; `GET/POST /api/batches`; `GET /api/batches/{id}` → `{batch, sessions}`;
  `POST /api/batches/{id}/accept {"message","push"}`; `POST /api/batches/{id}/discard`.
- Voice: `GET /api/voice` → `{available, backend, reason, secure_hint}`; `POST /api/voice/transcribe` (body = audio,
  Content-Type = mime, ≤ 25 MiB) → `{"text"}`.
- Status gains `vault_backend`, `voice: {available, backend}`, `tls: bool`.

## Web additions (`web/`)

- **Secrets view** (navbar tab "Secrets", phone: inside the Models tab as a section, or a 4th bottom tab
  if it fits at 360 px): list (name, note, length, updated), add or replace (password input, never echoed
  back), delete with in-page confirm, plus a note explaining the backend.
- **Usage view:** spend today and this month per provider, budget bars (ok / soft / hard colours from
  Branding status tokens), and a plan gauge per subscription (5 h window and weekly, with reset time; the
  limits are labelled "your estimate"; "set estimates in config" when 0).
- **History:** a search box at the top of Sessions, with filter chips (repo, agent, state); results show
  snippets with the match highlighted (safe text nodes, no innerHTML). Finished sessions get a **Resume**
  button (optional text) next to Discard.
- **Fleet tasks:** "New fleet task" on Sessions. It has a repo finder (has-file and name filters → checklist
  with select all and dirty badges), task text with a `{repo}`/`{name}` hint, agent, model and escalate
  choices, and creates a batch. The batch view shows a grid of repo × state chips, live, with buttons
  Accept all passed, Discard all, and per-repo links to the session.
- **Voice:** a mic button in the New task textarea and in the session follow-up bar. Press to record,
  press to stop (with a max-length timer shown). The transcript is inserted at the cursor for review,
  never sent automatically. When the page is not a secure context it explains "microphone needs HTTPS:
  see README (tailscale cert)", and reminds that the phone keyboard's dictation works anyway. Hidden when
  `/api/voice` says unavailable.
- Same rules as Phase 1: Carino look, mobile first, no polling while hidden, X-Cerberus header on writes,
  pure helpers unit-tested in web/test.

## Acceptance (Phase 2)

- Full gate: gofmt, vet, `go test -race ./...`, darwin and windows builds, `node --test`.
- Vault: a file-backend round trip, migration from secrets.json, values absent from every API response and
  log, redaction across split reads (unit test) and in a live session (a fake agent echoing a secret →
  scrollback shows `••••••`).
- Budget: a hard cap returns 429 from a fake provider, and a soft cap notifies once.
- Subs: parsing fixture JSONL with duplicate message ids, 5 h block computation, and an incremental re-read.
- History: search over 3 fake sessions, resume of a failed session runs a new attempt in the same worktree,
  and prune keeps a worktree with unaccepted work.
- Fleet: a batch over 3 temp repos (one with a failing check), with AcceptBatch → 2 accepted (main
  fast-forwarded, no branches), 1 skipped.
- Voice: a command backend with a fake script, an openai backend with an httptest server, and the ffmpeg
  conversion from a tiny generated wav (skip if ffmpeg is missing).
- Serve: listen retry binds once the address frees up (test with a port held, then released), and TLS
  serves with a generated self-signed cert.
- Idle RSS stays under 30 MB and idle CPU about 0% with subs in its idle mode.

## Phase 2 integration notes (as built)

Additions to the contract above, made while wiring the parts together:

- **Vault:** `Open(cfg, log, opts ...Option)`; `(*Vault).Lookup(name) string` for `engine.WithSecrets`.
  "auto" is sticky: it keeps the file vault when `vault.json` exists without `vault-index.json`, and
  fails (the daemon then runs without a vault) when only the keyring holds secrets and it does not
  answer. `Item` has no JSON tags (`Name`, `Note`, `Updated`, `Length`).
- **Voice:** the bearer token for a remote `voice.url` is the vault secret named `voice`.
  `GET /api/voice` also returns `max_seconds`; `secure_hint` is a string, empty over TLS.
- **Gateway:** `gateway.WithConfig(cfg)` supplies prices and budgets; `Budgets()` feeds
  `/api/usage`. Sub-cent caps are printed with significant digits ("$0.000001"), not "$0.00".
  Non-streamed replies report tokens per second over the whole request, and none under 50 ms.
- **Status:** `tls` means HTTPS is configured for the extra listen addresses (not that this request
  used it). `default_agent` is added so the task dialogs preselect `cfg.DefaultAgent`.
- **Session/fleet types** have no JSON tags; the API and web use Go field names. Batch accept and
  discard return a bare `[BatchResult]`; `POST /api/batches` returns 201 `{batch, sessions}`.

---

# Monitor phase specification

A serious view of the whole machine: what is running, what it uses, what it costs. Config: `internal/config/monitor.go`
(`Monitor`, `Tariff`, frozen add-only). All sampling is energy-aware: `ActiveIntervalMs` while a Monitor panel or
`cerberus top` is watching (`SetActive`, auto-expires 60 s after the last watcher), else `IdleIntervalSec` for the
energy history only (no per-process scan when idle).

## `internal/sysmon` (new): live system state

```go
type Monitor struct{ ... }
func New(cfg *config.Config, gpu *gpu.Monitor, log *slog.Logger) *Monitor
func (m *Monitor) Run(ctx context.Context)
func (m *Monitor) Watch() (release func())          // marks active; refcounted
func (m *Monitor) Snapshot() Snapshot                // last sample (never blocks on I/O)
func (m *Monitor) Processes() []Proc                 // last per-process sample (empty when idle)
func (m *Monitor) Signal(pid int, sig string) error  // "term" | "kill" | "stop" | "cont"; refuses protected
func (m *Monitor) Renice(pid, nice int) error         // 0..19 only (no privilege raising); refuses protected
```

- **CPU:** `/proc/stat` deltas → total and per-core %, `/sys/devices/system/cpu/cpu*/cpufreq/scaling_cur_freq` → MHz,
  load average, k10temp/coretemp `temp1_input` (Tctl/Tdie). **Memory:** `/proc/meminfo` (total, available, cached,
  swap, zram via `/sys/block/zram*/mm_stat`). **Network:** `/proc/net/dev` deltas (exclude lo, show per interface).
- **Disks:** mounts from `/proc/self/mountinfo` (real filesystems only; dedupe by device so `/` and `/home` on one
  btrfs show once with both mountpoints), `statfs` → size/used/free/%, `/proc/diskstats` deltas → read/write B/s
  and busy %, model from `/sys/block/*/device/model`, rotational flag, NVMe temperature from hwmon `nvme`.
  An HDD is "idle" when it had no I/O for 5 min (no root, no SMART: noted as such).
- **Processes** (active mode only): `/proc/[pid]/stat` CPU deltas, `statm` RSS, `comm` + `cmdline` (truncated 200,
  shown), owner uid, nice, state, start time; GPU per pid from `nvidia-smi --query-compute-apps=pid,used_memory
  --format=csv,noheader,nounits` plus `nvidia-smi pmon -c 1 -s u` for SM% (graphics apps too; tolerate absence).
  **Grouping**: by systemd unit or scope from `/proc/[pid]/cgroup` (`app-firefox-1234.scope` → "firefox",
  `cerberus-session-<id>` / agents.slice scopes → the Cerberus session id), with a "group" field the UI aggregates.
  Mark `cerberus` for Cerberus' own processes, sessions and the model servers it or LM Studio spawned.
  Send only the top 150 by CPU+RSS+GPU plus every Cerberus-related process.
- **Protected** (never signalled or reniced from the UI): pid 1, kernel threads, the user's systemd, the session
  leader and its compositor (Hyprland, gnome-shell, kwin, sway), dbus, pipewire/wireplumber, Xwayland, cerberus
  itself, plus `Monitor.Protected` names. Only processes owned by the daemon's uid can be acted on (the kernel
  enforces it anyway; check first for a clear error).

```go
type Snapshot struct {
    Time time.Time
    CPU  CPU   // Pct float64; Cores []float64; MHz []int; Load [3]float64; TempC float64; Model string; Threads int
    Mem  Mem   // TotalMiB, AvailMiB, CachedMiB, SwapTotalMiB, SwapUsedMiB, ZramOrigMiB, ZramComprMiB int
    Net  []Net // Name string; RxBps, TxBps float64
    Disks []Disk // Device, Model string; Mounts []string; FSType string; SizeB, UsedB, FreeB uint64; UsedPct float64; ReadBps, WriteBps float64; BusyPct float64; Rotational bool; TempC float64; Idle bool
    GPU  gpu.Snapshot
    Power Power // see energy
    Uptime time.Duration
}
type Proc struct { PID, PPID, UID int; Name, Cmd, User, Group, State string; CPUPct float64; RSSMiB int; GPUMiB int; GPUPct float64; Nice int; Started time.Time; Cerberus bool; SessionID string; Protected bool }
```

## `internal/energy` (new): watts, kWh, history, attribution

- **CPU watts:** RAPL `/sys/class/powercap/intel-rapl:0/energy_uj` (package; AMD Zen exposes it under intel-rapl)
  delta ÷ time, handling wrap with `max_energy_range_uj`. "auto" uses RAPL when readable, else the model:
  `CPUIdleW + (CPUMaxW − CPUIdleW) × util^1.3`, scaled by freq/maxfreq (documented ±15%).
  `Power.CPUSource` = "rapl" | "model".
- **GPU watts:** from `gpu.Snapshot` (measured). **Rest:** `BaselineW` (+5 W per spinning HDD while not Idle) and
  `ExtraW`. **Wall** = (CPU + GPU + rest) ÷ `PSUEfficiency` + ExtraW (ExtraW is already AC).
  `type Power struct { CPUW, GPUW, RestW, WallW float64; CPUSource string; Estimated bool }`.
- **History:** per-minute averages (CPU, GPU, rest, wall) appended to `DATA/energy/YYYY-MM.jsonl` in minute rows;
  hourly and daily rollups computed at read time and cached; files older than `HistoryMonths` deleted. On start,
  fill gaps as "off" (no energy). Downtime is not guessed.
- `func (e *Energy) Totals() Totals` → today, yesterday, last 7 days, month-to-date, last 30 days, last 12 months
  average per month (kWh, by component), plus `ProjectedMonthKWh` (month-to-date + last-7-day daily average ×
  remaining days).
- `func (e *Energy) Series(res string /* "minute"|"hour"|"day" */, since time.Time) []Point` for charts (≤ 2000 points).
- **Attribution:** per Cerberus session: CPU share = the session's cgroup `cpu.stat usage_usec` delta (systemd-run
  scope; else sum of its process tree from `/proc`) ÷ total CPU time × CPU watts, plus GPU energy from the usage
  records (`usage.Record.EnergyJ` where SessionID matches, i.e. the gateway already attributes model energy).
  Per model: the usage EnergyJ sum. Stored in `DATA/energy/sessions.json` (session id → joules CPU, joules GPU).
  `func (e *Energy) SessionEnergy(id string) (cpuJ, gpuJ float64)` and `ModelEnergy(since) map[string]float64`.

## `internal/bill` (new, pure): tariff maths

```go
func Monthly(t config.TariffConfig, kwh float64) Breakdown          // tiers + fixed + VAT for one month of kWh
type Breakdown struct { KWh, Energy, Fixed, VAT, Total float64; Tiers []TierLine; DAC bool }
func Marginal(t config.TariffConfig, pcKWhMonth float64) Marginal   // bill(H) − bill(H − pc); price per kWh at the margin
type Marginal struct { PCKWhMonth, HouseholdKWh, CostMonth, PerKWh float64; TierName string; DACRisk string /* "none"|"near"|"pushes"|"already" */; DACAvgKWh float64 }
func Cost(t config.TariffConfig, m Marginal, kwh float64) float64   // a session's or a day's kWh × the marginal price
```

- Household H = `HouseholdKWhMonth` (0 = unknown → use the PC's own kWh as H and say so). DAC: if the 12-month
  average (H) exceeds `DACLimitKWhMonth`, price everything at DAC; "pushes" when H − pc is under the limit but H is over.
  "near" when within 10%. All money rounded only for display.

## API, CLI, alerts

- `GET /api/monitor` → `{snapshot, power, totals, bill: {marginal, today_cost, month_cost, projected_month_cost, tariff_name, currency, verified}}`;
  `GET /api/monitor/processes` → `{processes, groups:[{name, cpu, rss_mib, gpu_mib, count, cerberus, session_id}]}`;
  `GET /api/monitor/series?res=&since=` → points; `GET /api/monitor/stream` (SSE `event: snapshot` each active tick;
  holds `Watch()` while connected); `POST /api/monitor/processes/{pid}/signal {"sig"}`, `/renice {"nice"}` (CSRF).
- Sessions: `session.Info` gains `EnergyJ float64` and `CostMXN float64` (filled on read from energy+bill);
  the session view shows them. Models list gains energy and cost per model (24 h / month).
- **Alerts** (through notify, rate-limited once per condition per 6 h): disk ≥ DiskPct, CPU/GPU/NVMe temps over
  limits, wall ≥ HighPowerW for HighPowerMin, projected month cost ≥ MonthBillMXN, DAC risk changes to "near" or "pushes".
- **CLI:** `cerberus top` (refreshing terminal view: CPU, RAM, GPU, power, top processes, sessions; q to quit; watches),
  `cerberus energy` (totals + bill summary), `cerberus rapl-setup` (prints, does not run, the one-time root setup:
  a udev rule `/etc/udev/rules.d/90-cerberus-rapl.rules` with
  `ACTION=="add", SUBSYSTEM=="powercap", KERNEL=="intel-rapl:*", RUN+="/usr/bin/chmod 0444 /sys%p/energy_uj"`
  plus the immediate `sudo chmod 0444 /sys/class/powercap/intel-rapl:*/energy_uj`; explains the trade-off; then
  checks readability and reports).

## Web: Monitor tab

- Navbar tab "Monitor" (phones: it fits as the 5th bottom tab only if 5 fit at 360 px; else Monitor replaces Usage in
  the bottom bar and Usage moves into Monitor as a section).
- **Overview:** big wall-watts number with CPU/GPU/rest split (badge "measured"/"estimated"), today kWh and
  cost, month-to-date and projected month cost with the marginal price per kWh and tier, a DAC risk chip, and an
  "approximate tariff, verify with your receipt" note until `Tariff.Verified`.
- **Charts:** power over the last hour (minute), 7 days (hour), 30 days (day), drawn as inline SVG (no library),
  stacked CPU/GPU/rest, colours from Branding data-viz tokens, values readable on hover/tap.
- **System:** CPU per-core bars + temp + MHz, memory (incl. zram), network, GPU card, disks (usage bar, I/O,
  temp, idle HDD), each compact on phones.
- **Tasks:** process table grouped by app (expand to pids), sortable by CPU/RAM/GPU, Cerberus sessions and models
  highlighted and linked; per-row menu: Terminate, Kill, Lower priority (with in-page confirm; protected rows
  show a lock and no menu).
- Live via `/api/monitor/stream` while visible; closed when hidden (so the daemon falls back to idle sampling).

## Acceptance

- Unit tests on fixture `/proc` and `/sys` trees (inject a root path), RAPL wrap, the CPU model, cgroup grouping,
  protected-process refusal, energy minute files and rollups (incl. month boundaries and gaps), bill maths
  (tiers, VAT, marginal, DAC "pushes"), alerts rate limiting.
- Idle cost of the monitor in background mode: one cheap sample every 30 s (no process scan), daemon idle RSS
  still < 30 MB; active mode with the Monitor open < 2% of one core.
- Full gate as before.

### Monitor phase: implementation notes (deviations and additions)

- `bill`: Go forbids a type and a function with the same name, so the result type of `bill.Marginal(...)` is
  **`bill.Margin`** (fields as specified, plus `HouseholdSet bool`). JSON is unaffected (`bill.marginal`).
- `sysmon.Power` lives in sysmon (energy aliases it) to avoid an import cycle; `sysmon.CPU` adds `MaxMHz` and
  `BusyTicks`, `Snapshot` adds `Active`. `sysmon.Monitor` adds `Touch()` (60 s lease for polls), `Active()`,
  `Groups()`, `Subscribe()`, `SetPowerFunc`, `AddHook`. Groups are JSON-tagged lowercase as the spec lists them.
- `GET /api/monitor/processes` adds `active`; `/api/monitor/series` returns a bare array of `{T, CPU, GPU, Rest, Wall}`
  (watts); `since` accepts RFC 3339, a Go duration or `Nd`. Minute points average over the seconds measured;
  hour and day points count gaps as off.
- `engine.ModelInfo` adds `EnergyJ24h, CostMXN24h, EnergyJMonth, CostMXNMonth` (wall-side, from gateway usage).
  `session.Info.EnergyJ` is wall-side (component joules ÷ PSU efficiency).
- Alerts live in `internal/alerts` (a sysmon hook). Usage gains `Store.OnAdd` (energy attribution of GPU joules).
- The projection divides the last-7-day energy by the time the data really spans (≥ 10 s), so a fresh install
  projects from its first minutes; restarts and downtime count as off.
- Signals: `term|kill|stop|cont`; renice only 0..19 and never below the current nice; protected = pid 1, kernel
  threads, the daemon and its ancestors, compositors and their ancestors, the built-in names + `monitor.protected`.
- `GET /api/monitor` (and each stream event, which is the same object) adds `alerts` (`config.AlertConfig`, so the
  UI colours disks and temperatures against the configured limits), and `bill` adds `dac_limit_kwh`.
- Energy history: a restart within a minute leaves two rows for that minute on disk; they are merged (weighted by
  seconds) in memory and in the minute series. Rows this run wrote before history loaded are skipped by count, not
  by time, so the previous run's last minute is kept. Hour and day points average over the time elapsed so far for
  the step still running (gaps before now count as off, the future does not).
- Web: the Monitor reads the API shapes above exactly (series `T/CPU/GPU/Rest/Wall`, `energy.Totals` entries as
  `{CPU, GPU, Rest, Wall}` kWh, model `EnergyJ24h/CostMXN24h/EnergyJMonth/CostMXNMonth`); hour and day charts say
  that downtime counts as 0 W.
- Review fixes: Signal/Renice build protection from a fresh read of the whole process table on every call (never
  the cached, top-150, idle-empty list): compositors with their ancestors and their session leader (`/proc` stat
  sid), the daemon by pid with its ancestors, session wrappers (`gdm-wayland-ses`, `gnome-session-b`, ...) by name.
  "cerberus" is protected by name only outside a session (`CERBERUS_SESSION` unset), so built-in agent sessions
  stay stoppable. A thread id is refused (a signal to it reaches the whole process).
- With `monitor.enabled` false, `GET /api/monitor/processes` and the process actions answer 404.
  `GET /api/monitor` stays passive unless `?watch=1` (the web view uses the stream; `cerberus top` sends it).
  Process command lines are masked with the vault values (and values pinned into live runs) before they leave.
- `sysmon.Disk` adds `Parent` (the whole disk); a spinning drive adds +5 W once, however many partitions it has.
- `bill.Margin.DACRisk` adds "unknown" when the household is not set (and the PC alone is under the limit);
  `energy.Totals` adds `ProjectionDays`; the bill and DAC alerts wait for 1 day of data behind the projection.
  Per-device alert titles name the device (the notifier rate-limits by title).
- Hour buckets follow the local clock (half-hour zones split days right); day series cover whole local days.
  Months past `HistoryMonths` also expire when a new month file starts. Sessions deleted by Prune are forgotten
  by energy (`session.WithForget`), and records of unknown sessions are dropped at load. Sessions without their
  own cgroup share one `/proc` scan per sample (ticks include reaped children).
- Web: on phones Usage is a collapsible "Usage & spend" section in Monitor (mounted only while open). Task groups
  show the server's complete per-app totals ("n of m shown" when expanded); model servers link to Models; focus
  survives refreshes; the chart x-axis is the selected window ending now; a tap keeps the chart tooltip up.


---

# Control center specification (day phase 1)

Watch and steer many agents at once, and let agents manage agents within limits.

## Session metadata and controls (`internal/session`)

- `Info` gains: `Name string` (user label, falls back to Title), `Tags []string`, `Pinned bool`, `Priority int`
  (−10..10; higher starts first in the queue; ties go by creation), `Limits Limits`, `Parent string` (the session
  that spawned it through MCP/API), `Depth int`, `LastOutput time.Time`, `OutputBytes int64`, and
  `Activity string` (last non-empty line of output, ANSI-stripped, ≤ 160 chars, updated at most 1×/s).
- `type Limits struct { MaxMinutes int; MemoryMax string; MaxOutputMiB int }` (0/"" = defaults). MaxMinutes:
  the session is stopped with reason "time limit" when exceeded (timer, no polling). MemoryMax: passed to the
  systemd-run scope (overrides Guard.SessionMemoryMax). MaxOutputMiB: stop when exceeded.
- `func (m *Manager) Update(id string, p Patch) (Info, error)` with
  `Patch{Name *string; Tags *[]string; Pinned *bool; Priority *int; Limits *Limits}`. It persists to meta.json and
  emits an event. Tags are lowercased, `[a-z0-9-]{1,24}`, ≤ 8.
- **Bulk operations:** `func (m *Manager) Bulk(ctx, ids []string, op BulkOp) []BulkResult` where
  `BulkOp{Kind string /* input|followup|stop|resume|accept|discard|checks|priority|tag|untag|pin|unpin */; Data string; Push bool; Message string; Priority int; Tag string}`
  and `BulkResult{ID, Result /* ok|skipped|error */, Detail string}`. Ops run sequentially, except input and
  followup, which run concurrently. Accept runs strictly sequentially. ≤ 100 ids.
- **Kill tree:** Stop already terminates the process group. Add `Kill(id)`, which sends SIGKILL to the group
  and the scope immediately, for stuck agents.
- TaskSpec gains `Name, Tags, Priority, Limits, Parent` (Parent set only by the MCP/API spawn path).
- The queue orders by (Pinned desc, Priority desc, Created asc).
- **Fallback:** when systemd-run exists but the user bus is unreachable (the scope fails to start), launch without the
  scope and log once, instead of failing the session.

## Activity feed (`internal/session`)

- `func (m *Manager) Feed(since time.Time, limit int) []FeedItem` with
  `FeedItem{Time time.Time; SessionID, Name, Kind /* created|state|waiting|passed|failed|accepted|followup|input|spawned|limit */, Text string}`.
  It's kept in memory (last 2000) and appended to `DATA/feed.jsonl` (rotated at 4 MiB). Every state change and every
  user action through the API is recorded, with input contents clipped to 80 chars and secrets redacted through the
  vault values.

## Agents managing agents

- `cerberus mcp` is a stdio MCP server (JSON-RPC 2.0, MCP protocol versions "2026-07-28" and "2025-06-18", tools capability only; see the Flows "As built" note)
  that talks to the local daemon's API with the session's agent token (`CERBERUS_URL` + `OPENAI_API_KEY`, the agent
  token, which now ALSO grants a restricted `/api/agent/*` scope; the full admin token is never given out).
- Tools:
  - `cerberus_spawn {task, agent?, model?, workspace?: "scratch"|"same", name?}` creates a child session. "same"
    means a folder/scratch session in the parent's own workspace dir (a repo child gets its own worktree of the
    same repo).
  - `cerberus_list {}` returns the caller's children.
  - `cerberus_read {id, tail_kib?}` returns the transcript tail (≤ 64 KiB).
  - `cerberus_send {id, text}` sends a follow-up.
  - `cerberus_wait {id, timeout_s≤600}` blocks until the child leaves running/waiting.
  - `cerberus_stop {id}`.
- Restricted scope (enforced server-side, keyed by the session derived from the agent token + `X-Cerberus-Session`):
  - a session may only see and steer its own descendants;
  - spawn limits come from `cfg.Agent.Spawn` (`SpawnConfig{Enabled (default true); MaxDepth (2); MaxChildren (4); MaxTotal (8); RequireApproval (false)}`, in internal/config/spawn.go);
  - with RequireApproval set, the child is created in state `queued` with WaitingReason "needs approval", and the UI
    shows Approve/Reject (`POST /api/sessions/{id}/approve`).
- The built-in agent exposes the same six cerberus_* operations as extra tools in its own tool loop when
  CERBERUS_URL is set and spawn is enabled (calling the API directly). Other agents (Claude Code, OpenCode) use
  `cerberus mcp`: the claude profile gets `--mcp-config` pointing at a generated `DATA/sessions/<id>/mcp.json` when
  `cfg.Agent.Spawn.Enabled`.
- The agent token's restricted scope must not reach any non-agent endpoint except /v1 (tests must prove it).

## API

- `PATCH /api/sessions/{id}` (Patch JSON), `POST /api/sessions/bulk {ids, op}`, `POST /api/sessions/{id}/kill`,
  `POST /api/sessions/{id}/approve {"approve": bool}`, `GET /api/feed?since=&limit=`, `GET /api/feed/stream` (SSE).
- Agent scope: `POST /api/agent/spawn`, `GET /api/agent/children`, `GET /api/agent/sessions/{id}/tail`,
  `POST /api/agent/sessions/{id}/send`, `GET /api/agent/sessions/{id}/wait?timeout=`, `POST /api/agent/sessions/{id}/stop`.
- `GET /api/sessions/{id}/stream` is unchanged. New `GET /api/wall/stream?ids=a,b,c` is ONE SSE multiplexing several
  sessions' output (`event: out` with data `{"id":..,"b64":..}`; backlog limited to 8 KiB each, ≤ 16 ids), so a wall
  of N terminals uses one connection.

## Web: Control center

- **Wall view** (`#/wall`, a navbar tab "Wall"): a grid of live mini terminals (xterm instances, read-only until
  focused) for the selected or all-active sessions. The layout auto-fits (1/2/4/6/9), and the user can pin which ones
  show. Each tile has a header (name, state chip, agent and model, elapsed, energy) and a footer with the last activity
  line. Tap a tile to focus it full-size with the input bar (reusing the session view components). On phones it's one
  tile per screen with horizontal swipe and a dot indicator. It uses the single multiplexed SSE.
- **Sessions list:**
  - multi-select checkboxes, plus a bulk bar (Input, Follow-up, Stop, Resume, Accept, Discard, Run checks, Priority,
    Tag, Pin) with in-page confirms for destructive actions and a result toast summary;
  - inline rename, tag chips with a tag filter, pin to the top, a priority stepper;
  - limits editor in the session view;
  - parent/child tree indentation for spawned sessions, and Approve/Reject for pending spawns.
- **Broadcast input:** in the bulk bar, the text is sent as input or follow-up to all selected sessions (quick keys too: y, n, Enter, Ctrl-C).
- **Activity feed:** a collapsible side panel on desktop (a drawer on phones) with the live feed and filters by kind;
  clicking an item opens the session.
- **Keyboard shortcuts (desktop):** `g w` wall, `g s` sessions, `n` new task, `/` search, `j/k` move selection,
  `x` select, `.` open the bulk bar, `?` help overlay. None fire while typing in inputs or a focused terminal.
- Bottom tabs on phones: keep at most 4 (Sessions, Wall, Monitor, More); "More" opens a sheet with Models, Usage,
  Secrets and any later views.

## Acceptance

- Unit and e2e: Patch persistence, priority queue order, bulk ops (incl. accept sequencing), time-limit stop,
  MaxOutputMiB stop, feed redaction, MCP stdio round trip (initialize → tools/list → spawn → wait → read) against a
  test daemon with a fake child agent, restricted-scope enforcement (an agent token cannot list non-descendants,
  cannot call /api/sessions, cannot exceed depth or children limits), approval flow, multiplexed wall stream, and the
  no-user-bus fallback.
- Screenshots of the wall (desktop 4 tiles, phone swipe), bulk bar, feed and tree at 1280 and 390.

## Control center: implementation notes (as built)

Deviations from and additions to the specification above:

- **Feed kinds:** one extra kind, `update`, records metadata changes (rename, tags, pin, priority,
  limits) and "Save to folder" ("saved N files to DIR", or "saved N of M files" when it stopped
  partway). Approve and reject are `state` items with the text "approved" / "rejected". Terminal
  input (session view and bulk/broadcast alike) is recorded per line ("text ⏎" or a key name such as
  Ctrl-C), never per keystroke; a line any part of which was typed while the terminal had echo off (a
  password prompt; read from the PTY's termios) reads "(hidden input) ⏎", and so does any input where
  echo cannot be read. Text is masked through the vault values first and clipped (80 / 240
  characters) after, so a secret is never cut in two. Spawns read "by <parent name>: <task>".
  FeedItem JSON keys are Go field names, like Info.
- **Limits are per run:** MaxMinutes counts from each launch and MaxOutputMiB counts that run's
  bytes, so a retry, resume or follow-up starts fresh (`OutputBytes` in Info stays the total). A
  limits change reaches a running agent at once for time and output; MemoryMax applies from the next
  run. "" MemoryMax means `guard.session_memory_max`.
- **Spawn counting:** the limits count open sessions (not accepted or discarded). MaxChildren counts
  the parent's open direct children, MaxTotal the open sessions below the root a person started.
  Depth is 0 for a person's session. Rejecting a pending spawn discards it. A pending spawn survives
  a daemon restart (meta.json `needs_approval`) and is not stopped by shutdown. Stopping or killing
  a pending spawn keeps it unapproved (stopped, "stopped before approval"): it can only be
  discarded; follow-up, resume and checks refuse it, also after a restart.
- **Per-session agent token:** `cas-<id>.<hex HMAC-SHA256(server token, "cerberus session agent
  token v1\0" + id)>`. The id travels inside the token because `/v1` clients send no session header;
  on `/v1` the token pins `X-Cerberus-Session` to its own id. `/api/agent/*` takes only this token,
  plus an `X-Cerberus-Session` header equal to the id inside it: a missing or forged token is 401;
  the admin token, the cookie or the shared `cag-` token is 403; a missing or different header, or a
  caller that is unknown, accepted or discarded, is 403. Ids outside the caller's tree are 404. The
  token is 401 on every other `/api` route, including `?token=`.
- **Agent scope API:** `GET /api/agent/children` returns every descendant (with Parent filled in).
  Spawn workspaces are "scratch" (default) or "same"; anything else is 400, and a spawn limit is 403.
  A spawned task or model, or a `send` text, that starts with "-" is 400 (it would become an option of
  the child's agent CLI). `send` only starts a new run of a finished or stopped child; a child with a
  run (running, waiting, starting…) is 409, so an agent never types into a child's terminal or
  answers its approval prompts.
  `wait` treats queued (including a pending approval), starting, running, waiting, checking, retrying
  and escalated as still working; `timeout` defaults to 60 s, max 600. `tail` returns plain text, at
  most 64 KiB.
- **Bulk:** more than 100 ids, an unknown kind, a bad priority or tag, or empty text marks every id
  `error`. An op that doesn't apply (stop of a finished session, accept while running, ErrBusy) is
  `skipped`. The HTTP reply is `{"results": [...], "ok", "skipped", "error"}`, and the bulk runs
  detached from the request (30 min cap).
- **Wall stream:** backlogs arrive as `event: out` with `"backlog": true` (one per session, ≤ 8 KiB),
  then live `out` events. The stream ends when any member's stream ends; the client reconnects and
  clears each tile before its backlog. Mini terminals keep the session's PTY width and scale their
  font to the tile; only the session view resizes the PTY. A focused tile on a phone keeps an 8 px
  minimum font and scrolls sideways.
- **Scope fallback:** each scope gets `--unit=cerberus-<id>-<attempt>-<rand>` so Kill can also
  `systemctl --user kill --signal=SIGKILL` it. A scoped run that exits non-zero within 5 s with a bus
  or transient-scope error in its output only raises the question: a probe scope
  (`systemd-run --user --scope -- true`) decides. When the probe fails too, scopes are off for the
  daemon's lifetime (logged once) and the attempt is rolled back and relaunched without a scope,
  with a note in the terminal; when it starts, the run simply failed. `/api/status` `guard` gains
  `memory_enforced` and `session_memory_max`; with caps not enforced the session view reads
  "4G memory (not enforced)" and the limits dialog says so. In the dialog an empty time or output
  limit means no limit, an empty memory cap means `session_memory_max`.
- **MCP:** `cerberus mcp` handles `tools/call` concurrently (a long wait doesn't block ping), honours
  `notifications/cancelled`, and caps a message at 8 MiB. `--mcp-config` is added only for profiles
  whose binary is `claude`, only with spawning on, and only when the argv has none. The built-in agent
  offers the cerberus_* tools only when CERBERUS_URL and a session id are set and spawning is on; in
  ask mode, spawn and stop need approval.
- **Web extras:** `#/wall/a,b,c` shows exactly those sessions; plain `#/wall` shows kept tiles first,
  then active sessions (waiting first), at most 9. Escape inside a focused tile goes to the agent;
  leave with "← Wall". Kill (with a confirm) is in the session view; Unpin sits next to Pin in the
  bulk bar; the New task form takes tags and priority; Enter opens the highlighted row. A session
  event that changes nothing a list row is built from (only Activity, output counters) patches the
  row's activity line in place, and no row is rebuilt while a pointer is down in the view, so clicks
  are never lost; an unsent priority step survives a rebuilt editor. Wall tiles keep the elapsed
  time at every width (the agent/model text is cut instead). Feed times refresh every 30 s while
  the panel is open and the tab visible.


---

# File drop specification (day phase 2)

Give files to agents and take files back, from the desktop or a phone: drag and drop, pick, paste,
or hand over from another Carino page.

## Storage model (`internal/files`, new)

- Every session has a workspace (repo worktree, plain folder or scratch). Uploads go **into the workspace** under
  `<workspace>/<UploadDir>/` (config `Files.UploadDir`, default `inbox`). A repo session's inbox is added to the
  worktree's `.git/info/exclude`, so uploads never land in an accepted commit unless the agent moves them out.
- **New-task attachments:** files dropped on the New task form go to `DATA/uploads/<staging-id>/` and move into the
  session's inbox at Create. Staging expires after 24 h.
- **Chat attachments** (later phase) reuse the same staging.
- New config `FilesConfig` (internal/config/files.go):
  `{UploadDir string ("inbox"); MaxFileMiB int (200); MaxTotalMiB int (2048 per session); AllowExec bool (false); DenyExt []string (default none); KeepNames bool (true)}`.

```go
type Store struct{ ... }
func New(cfg *config.Config, sessions *session.Manager, v *vault.Vault, log *slog.Logger) *Store
func (s *Store) Put(ctx context.Context, target Target, name string, r io.Reader, size int64) (Entry, error)
func (s *Store) List(target Target, rel string) (Listing, error)          // one folder level, folders first
func (s *Store) Open(target Target, rel string) (*os.File, Entry, error)   // download/preview
func (s *Store) Delete(target Target, rel string) error                    // files only inside the workspace, never outside; folders only if empty unless Recursive and inside inbox
func (s *Store) Mkdir(target Target, rel string) error
type Target struct { SessionID string; Staging string }                    // exactly one set
type Entry struct { Name, Rel string; Size int64; Mod time.Time; Dir bool; Mime string; Text bool; Image bool }
type Listing struct { Rel string; Parent string; Entries []Entry; Truncated bool }
```

- **Safety:**
  - Names are sanitised: NFC; strip path separators, NUL, control chars, leading dots and `..`; ≤ 180 bytes;
    collisions become `name (2).ext`.
  - All paths are resolved with symlink checks to stay inside the workspace (Rel never escapes; a symlink pointing
    outside is shown but refused for Open, Put and Delete).
  - Writes go to a temp file in the same dir, then rename. Files are created 0644 (or 0600 when the name matches
    secret-ish patterns, e.g. *.pem, *.key, id_*, .env). The exec bit is **never** set unless `AllowExec`.
  - Size limits are enforced while streaming (no full buffering in memory).
  - Writes to deny paths (agent.DeniedBy) are refused.
  - Listing skips `.git`.
- **Audit:** every Put and Delete is appended to the activity feed (kind `file`) with name, size, session and
  client address, and also to the audit log once phase 3 exists.

## Interop

- **Built-in agent:** at start (and on each follow-up), a note lists new inbox files ("The user added files to
  ./inbox: a.png (34 KB), notes.txt (2 KB)"). Images among them are attached as image parts for vision models when
  the model supports images (fall back to text mention on refusal, as Atrium does).
- **Other agents:** the follow-up text mentions the paths (e.g. "Files added: inbox/a.png").
- **Carino Bridge:** copy `carino-bridge.js` verbatim from ~/Github/Files/carino-bridge.js into web/dist/assets/.
  Cerberus can **receive** (`#carino-bridge` on the UI URL opens a "Choose session or new task" sheet for the
  received files; allow list: origins under `https://*.carino.systems` plus same-origin; others → ask) and
  **send** (from the workspace file browser, "Open in Carino Files" hands the file to files.carino.systems).
  Never label a handoff by trust, only by origin.

## API

- `POST /api/sessions/{id}/files?dir=REL` (multipart, several files; streaming) → `[Entry]`
- `GET /api/sessions/{id}/files?dir=REL` → Listing; `GET /api/sessions/{id}/files/raw?path=REL[&download=1]` → the
  file (Content-Type from extension sniffing, `X-Content-Type-Options: nosniff`, CSP sandbox for HTML/SVG,
  `Content-Disposition: attachment` for anything not safe to inline); `DELETE /api/sessions/{id}/files?path=REL`;
  `POST /api/sessions/{id}/files/mkdir {"path"}`.
- `POST /api/uploads` (multipart) → `{staging, entries}`; `DELETE /api/uploads/{staging}`; TaskSpec gains `Staging string`.
- All writes require CSRF and auth; the agent scope (phase 1) may list and read its OWN session's files only.
- Request body limits follow FilesConfig, with a clear 413 JSON error.

## Web

- **Drop anywhere:** dragging files over the window shows a full-screen drop target naming where they will go (the
  open session, or "new task" on Sessions). It also has a picker button (`<input type=file multiple>`, on phones
  this offers camera and gallery) and clipboard paste of images and files into the session input bar and the New
  task form.
- **Upload progress** per file (XHR upload progress; cancel), with a summary toast and errors per file (too large,
  refused name).
- **Workspace browser** in the session view (a "Files" tab beside Terminal and Diff):
  - folder navigation and breadcrumbs;
  - preview for images, text and code (monospace, first 256 KiB), PDF (via the browser's own viewer in a sandboxed
    iframe), audio and video;
  - download, delete with confirm, new folder, and "Send to agent" (a follow-up naming the file);
  - "Open in Carino Files" through the bridge.
- **New task form:** an attachments area (drop, pick or paste), with chips showing name and size and a remove button.
- Phones: the picker is the primary path, and the drop overlay is desktop-only.

## Acceptance

- Unit: name sanitising table, traversal and symlink refusal, size limits mid-stream, exec bit never set,
  collision naming, .git/info/exclude entry, staging → inbox move, expiry.
- API: multipart streaming upload of a 50 MiB file with RSS staying flat (measure), 413 over the limit, agent-scope
  isolation, raw download headers (nosniff, sandbox CSP for HTML/SVG).
- Built-in agent: an image in the inbox reaches a fake vision server as an image part.
- Screenshots: the drop overlay, upload progress, the Files tab with an image preview, text preview, New task
  attachments, the phone picker sheet, and the bridge receive sheet.

## File drop: implementation notes (as built)

Deviations from and additions to the specification above:

- **Store API additions:** `PutIn(ctx, target, dir, name, r, size)` puts into a chosen existing folder
  (`Put` = `PutIn` with dir "" = the inbox, or a staging root). `RemoveAll` is the recursive delete,
  only for folders inside the inbox (anywhere in a staging folder); `Delete` removes files, links
  (never their target) and empty folders. `As(client)` is a view that adds "· from ADDR" to feed
  items. Also `NewStaging`, `DeleteStaging`, `Sweep(now)`, `Run(ctx)` (sweep at start, then hourly
  on a timer) and `UploadsDir`. Errors map to HTTP: ErrTooLarge 413, ErrRefused and ErrOutside 403,
  ErrNotFound 404, ErrIsDir and ErrBadTarget 400, ErrNotEmpty 409; a session state that refuses files
  (repo session without a worktree yet, accepted, discarded) is 409.
- **Entry and Listing** gain `Link` and `Outside`; JSON keys are Go field names. Listing `Rel` is "."
  at the root, `Parent` is "" at the root and "." for a top-level folder; at most 2000 entries, then
  `Truncated`.
- **Names:** the last path part is kept (`../../etc/passwd` → `passwd`, `C:\a\b.txt` → `b.txt`);
  characters Windows refuses become "_", bidi overrides and BOMs are removed, Windows device names get
  a "_" prefix. `.env` is stored as `env` but still gets 0600 (the original name is checked too).
  NFC is minimal: a base letter plus one combining mark for Latin-1 Supplement and Latin Extended-A;
  other scripts are kept as sent (documented in `internal/files/fsafe/nfc.go`).
- **Exec bit:** uploads carry no mode, so with `AllowExec` only content starting with `#!` gets 0755
  (0700 for secret-ish names). Without it the bit is never set, and staged files lose it on the move.
- **Totals:** `MaxTotalMiB` counts every byte uploaded to a session (`Info.UploadBytes`, persisted;
  deletes do not give bytes back); for a staging folder it is the folder's current size. Concurrent
  uploads share an in-flight counter. The whole request body is capped at `MaxTotalMiB` + 1 MiB of
  multipart room; a `Content-Length` over what is left is refused before reading.
- **Exclude entry:** git reads `info/exclude` of a linked worktree from the shared git dir, so the
  file is found with `git rev-parse --git-path info/exclude` and gets an anchored `/inbox/` line once.
  New untracked `inbox/` files in the main checkout of that repo are hidden too; tracked files are
  not affected. Only repo sessions get it.
- **Staging:** `TaskSpec.Staging` must be lowercase hex (16 to 64 characters) naming an existing
  folder, else Create fails ("the attachments expired or were removed"). With Count > 1 earlier
  sessions get real copies (never hard links, so no session can change another's files) and the
  last one takes the folder. A Create claims the staging id first, so a second Create on it fails
  before touching any file; a Create that fails puts every file back into the staging folder, so a
  retry gets the full set. Delivery skips uploads still in progress (`.cerberus-upload-*.part`) and
  keeps a staged 0600 file private. For a repo session the files are delivered when its worktree is
  made. The feed reads "attached N files: a.png (34 KB), …".
  `POST /api/uploads?staging=ID` adds files to an existing staging folder; `GET /api/uploads/{id}`
  lists one. A new staging folder whose first upload fails is removed.
- **Mentions:** paths the Store adds are kept per session (meta.json `pending_files`); the next
  follow-up (and the first task, for staged files) gets "\n\nFiles added: inbox/a.png, …" for
  agents other than the built-in one. Typed into a running interactive agent, the sentence goes in
  parentheses on the same line. `Info.Task` and the feed keep the user's own text.
- **Built-in agent:** each run walks `<cwd>/<upload_dir>` (no symlinks, dot folders or deny_paths),
  compares against `$CERBERUS_SESSION_DIR/inbox-seen.json` (size and mtime) and adds the note to the
  run's user message. New png, jpeg, gif or webp files (at most 4, 5 MiB each) go as `image_url`
  data-URL parts unless `GET /v1/models` lists the model as local without `vision`. A 400/415/422/500
  that mentions image, vision, multimodal or array (or says nothing), never a context-size error,
  401, 403 or 429, removes the image parts, notes that they could not be shown, and retries.
  Messages with images marshal to OpenAI content parts, so `--resume` keeps them.
- **Raw files:** `text/*` gets `; charset=utf-8`; HTML, XHTML, SVG and XML get
  `Content-Security-Policy: sandbox; default-src 'none'; style-src 'unsafe-inline'; img-src data:;
  media-src data:` and show inline; images, audio, video, text, PDF and JSON show inline; everything
  else, and anything with `download=1`, is an attachment (RFC 2231 file names). Range works through
  `http.ServeContent`. Also `Cross-Origin-Resource-Policy: same-origin` and `Cache-Control: private,
  no-cache`. PDFs are not CSP-sandboxed (a sandbox CSP can stop the browser's PDF viewer).
- **Agent scope:** `GET /api/agent/files?dir=REL` and `GET /api/agent/files/raw?path=REL` take no
  session id: the token's own session is the only one they can name. No MCP tools for them yet.
- **Status:** `/api/status` gains `files: {upload_dir, max_file_mib, max_total_mib}`; the web UI
  names the real inbox folder and refuses too-large files before sending.
- **Web:** session uploads go one file per request, queued one at a time, each with its own progress,
  cancel and error; `dir=.` is the workspace root (the Files tab uploads into the folder on screen).
  New task attachments are checked against `max_file_mib` when added, stay in the browser until
  Start, then go one file per request (`POST /api/uploads`, then `?staging=ID`), each chip with its
  own progress and error; "Cancel upload" stops it and keeps the form; pressing Start again reuses
  what is staged and sends the rest, and closing the form deletes it. Inside a folder the Files tab
  has "Delete folder" (with contents inside the inbox, empty folders elsewhere). Reading a workspace
  (list, preview, download, delete) creates no inbox and runs no git; only writes into the inbox
  make it. The Diff button became a Diff tab (repo sessions only); `#/s/{id}/files` opens the Files
  tab, and switching tabs uses `history.replaceState` so the terminal is not remounted. The drop
  overlay shows only for a hovering fine pointer and moves into the topmost open dialog; Esc closes
  the overlay without closing the dialog under it. The upload tray sits above the input bar. The PDF
  preview is an iframe with `sandbox=""` plus an "Open the PDF in a new tab" link, because Chromium
  does not render a PDF inside a sandboxed frame.
- **Bridge:** receive uses `allow: ['same-origin']` with `otherwise: 'ask'`; the ask callback lets
  `https://*.carino.systems` through without a question and asks about any other origin. The files
  are acknowledged at once, then the "Choose session or new task" sheet opens, labelled by the
  sender's host. Sending fetches the file only once the receiver is listening. A receiver that only
  accepts an exact allow list (and Cerberus sends `Referrer-Policy: no-referrer`) never answers a
  self-hosted Cerberus origin; the sender then reports a timeout after 30 s.
- **Scope fallback fix:** a scoped run's first output is held back only while it could still be
  systemd-run's own "Failed to …" message; when the no-scope fallback takes over, the held text is
  dropped so the transcript keeps only the one-line note.


---

# Safety specification (day phase 3)

Cerberus can run commands on its host. This phase makes the safety story explicit, checkable and
hard to get wrong.

## Threat model (`docs/SECURITY.md`, new)

- Assets: the host account, repos and folders, secrets in the vault, the API token, model and API spend, other LAN
  devices.
- Actors: the owner (trusted), devices the owner paired (trusted per device), agents (semi-trusted: they act for the
  owner but run model output), web pages in the owner's browser (untrusted: CSRF, DNS rebinding), other hosts on the
  LAN or tailnet (untrusted unless paired), uploaded files and preview pages (untrusted content).
- For each: entry points, existing controls (link to code), residual risk, and what the owner must do (e.g. keep it
  off the public internet). Document what Cerberus does NOT protect against (an agent with shell access can read
  anything the user can unless sandboxed; the file vault backend vs local attackers).

## Audit log (`internal/audit`, new)

- `DATA/audit.jsonl`, append-only, 0600, rotated at 8 MiB (keep 5). Records every state-changing API call and CLI
  action: time, actor (`owner:<device>` | `agent:<session>` | `cli` | `system`), remote address, method, route
  pattern (not the raw URL with query), target ids, result status, and a short detail. Bodies are never stored;
  inputs and follow-ups are clipped to 80 chars and redacted through vault values.
- Also logged: auth failures, token rotations, device pairing and revocation, sandbox fallbacks, and kill/renice of
  host processes.
- `GET /api/audit?since=&actor=&limit=` and `cerberus audit [--since 24h] [--actor X]`. The UI shows it under
  Settings → Security (phase 4) or a temporary Audit view.

## Authentication hardening (`internal/api`)

- **Per-device tokens:** `cerberus pair [--name phone]` creates a device token (random 32 bytes, stored hashed with
  SHA-256 in `devices.json` 0600 with name, created, last-seen, last address). It prints the pairing URL (with the
  token) and an ASCII QR code (a small QR encoder, byte mode, ECC M, written in-house: no dependency). The admin
  token keeps working. `GET/DELETE /api/devices` list and revoke devices; revoking ends their SSE streams at once.
  Cookies carry the device token.
- **Token rotation:** `cerberus token rotate` (and POST /api/token/rotate) replaces the admin token, rewrites the
  config, invalidates cookies made from the old one, and re-derives agent tokens (live sessions get new tokens
  through their next spawn; existing ones keep /v1 only until they end).
- **Rate limits:** a token bucket per remote IP for auth failures (5 per minute, then 429 for 10 min, logged), and a
  global cap on concurrent SSE streams per actor (32).
- **Bind safety:** refuse to start when any listen address is a public, non-private IP (not loopback, RFC1918,
  CGNAT 100.64/10, ULA fc00::/7 or link-local) unless `Server.AllowPublic` (new) is set. Warn loudly for 0.0.0.0.
- **Host header check** on every request (not only /v1): it must match a listen address, PublicURL host,
  localhost, or a configured `Server.AllowedHosts` (new), defeating DNS rebinding for the UI too.

## Session sandbox (`internal/sandbox`, new)

- `AgentProfile.Sandbox` (new): `"" | "none" | "bwrap"`, plus `RepoConfig.Sandbox` override and
  `Sandbox SandboxConfig` defaults (new config): `{Default string ("none"); Network bool (true); ReadOnlyHome bool (true); ExtraRO, ExtraRW []string; HideHome []string (default ~/.ssh, ~/.gnupg, ~/.config/cerberus, ~/.local/share/keyrings, ~/.mozilla, ~/.config/google-chrome)}`.
- With bwrap (if `bwrap` is on PATH; else fall back to none with a feed and audit warning, or refuse when
  `Sandbox.Strict`):
  - `--ro-bind / /`, `--dev /dev`, `--proc /proc`, `--tmpfs /tmp`;
  - `--bind <workspace> <workspace>`, `--bind <session dir>`;
  - each HideHome path replaced with an empty tmpfs (`--tmpfs`);
  - `--unshare-net` when Network is false (the gateway is then unreachable, so local models via the gateway need
    Network true; document it);
  - `--die-with-parent`, `--new-session`.
- Agent CLIs that need their own config (claude reads ~/.claude) get `--bind ~/.claude ~/.claude` through the
  profile's `ExtraRW`, and the default claude profiles carry it.
- Composes with the systemd-run scope (scope outside, bwrap inside).

## Built-in agent command policy (`internal/agent`)

- `Agent.Policy` (new config): `{Allow []string; Deny []string; AskAlways []string}` as command prefix patterns
  matched against the parsed first word(s) of each pipeline segment (split on `|`, `&&`, `||`, `;`, with simple
  quote handling). Deny wins and is never askable.
- Default deny: `rm -rf /`, `rm -rf ~`, `mkfs`, `dd of=/dev/`, `:(){`, `shutdown`, `reboot`, `systemctl --user stop cerberus`,
  `curl … | sh` style pipes into a shell, `git push --force`, `git branch`, `git checkout -b`, `git switch -c`.
- Default AskAlways (asked even in auto mode): `git push`, `sudo`, `npm publish`, `pip install`, `dnf`, `rm -r`.
- Applies to run_command only (file tools keep their path rules).

## `cerberus doctor`

- A checklist run from the CLI (`cerberus doctor [--json]`) and `GET /api/doctor`, with each check returning
  ok, warn or fail, a one-line explanation and a fix:
  - config validity, file modes (config, vault, devices, audit), listen addresses (public?), TLS presence for
    non-loopback, token age;
  - LocalNoAuth, sandbox availability (bwrap), RAPL readability, engines reachable, agents installed,
    systemd-run and user bus;
  - disk space for DATA, retention, the git version (worktree support), ffmpeg/voice, and the service unit installed.
- The UI shows doctor results as a card on Monitor (counts) and fully under Settings → Security later.

## Acceptance

- Unit and e2e for the audit log (redaction, rotation, route patterns), device pairing, revoke cutting live SSE,
  rotate, rate limit, public-bind refusal, Host check, QR encoder against known vectors, bwrap argv construction
  (and a real bwrap run in a test when bwrap exists, skipped otherwise), policy parser (table of bypass attempts:
  `r''m -rf /`, `$(echo rm) -rf /`, `bash -c 'rm -rf /'`, env-prefixed commands, …; documented limits:
  the policy is a guard rail, not a sandbox), and doctor checks with fixture states.
- Adversarial review lens focused on bypassing auth, the agent scope, the sandbox and the policy.

## Safety: implementation notes (as built)

Deviations from and additions to the specification above. The owner-facing story is in
[SECURITY.md](SECURITY.md).

- **Packages:** `internal/audit`, `internal/sandbox`; the device store, QR encoder and doctor are
  subpackages of the API (`internal/api/devices`, `internal/api/qr`, `internal/api/doctor`, which also
  holds `CheckListen` for `serve`). The handler chain is headers → Host check → auth (rate limit,
  principal, stream cap, revocation) → audit → mux, so audit records carry `r.Pattern`.
- **Config (add-only):** `Server.TokenCreated` (`token_created`, set on first run and on rotation; the
  doctor prefers it over the newest audited rotation). Token reads and writes go through
  `Config.TokenSnapshot` / `SetToken` (one lock for all configs), so a rotation never races the session
  manager deriving agent tokens or a concurrent `Save`. Default `SandboxRW` for the claude profiles is
  `~/.claude` and `~/.claude.json`. Default deny also holds `systemctl reboot`, `systemctl poweroff`,
  `poweroff` and `halt`.
- **Host check:** failures answer 403 (an OpenAI-style error on `/v1`). Under a wildcard listen any IP
  literal is accepted too: rebinding needs a name.
- **Rate limit:** only requests presenting a wrong credential count; the same bad value from one address
  counts once and a bad cookie is cleared. The block covers every non-public path from that address (an
  IPv6 /64 is one address), even with the right token. Records: actor `system`, route `auth.failure` or
  `auth.blocked`, the mux pattern as the target. CSRF 403s are not audited.
- **Audit middleware** skips `/v1/*` (usage records it) and `POST /api/usage/watch`. Handler notes are
  the detail (input and follow-up text, accept message, resume text); hidden terminal input is
  "input (hidden, N bytes)". Kill and renice write the HTTP record plus a `process.signal` /
  `process.renice` event with pid, name and outcome. `serve` writes `serve.start`.
- **Devices and rotation:** pairing needs the admin token; devices can list and revoke. The pairing reply
  is `{device, token, url, qr}` (`qr` as rows of `#`/`.`); `GET /api/devices` is `{devices, current}`
  where `current` is the caller's device id or `admin`. Rotation is owners only; the new token is
  returned to an admin bearer caller only, an admin-cookie caller gets a fresh cookie. It is refused when
  the config has no file path. Up to 8 rotated-away tokens are kept in memory so live session agent
  tokens keep `/v1` until their session ends; they never open `/api/agent`. The shared agent token and
  preview capability URLs change at once.
- **CLI extras:** `cerberus devices [list | rm ID|NAME]`; `pair --name --url --light-terminal`;
  `audit --limit --json`. `audit` reads `DATA/audit.jsonl` directly; pair, devices, rotate and local
  `secret set/rm` go through the daemon when it runs, else change the files and audit as `cli`. CLI
  requests send `X-Cerberus-Actor: cli`, honoured only with the admin bearer token.
- **Doctor:** 16 checks (config, modes, listen, tls, token_age, local_no_auth, sandbox, rapl, engines,
  agents, systemd, disk, retention, git, voice, service). `cerberus doctor` never creates a config.
- **bwrap argv** adds `--unshare-pid` (with `--new-session` the agent leaves the PTY's process group, so
  stop and kill reach it only through bwrap's death), `--chdir <workspace>`, `--ro-bind /dev/null` for a
  HideHome entry that is a file, skips binds of missing paths, and sets `TMPDIR=/tmp`. Cerberus' data and
  config folders and `$XDG_RUNTIME_DIR` are always hidden; the session folder and workspace are bound
  back after the hides. Unknown sandbox words mean bwrap (fail closed). Checks from `.cerberus.json`
  run in the same sandbox (not at all under Strict without bwrap). Limits: the repo's `.git` is
  read-only inside (commit on the host), the home is read-only (`ExtraRW` for `~/.cache`), there is no
  controlling tty, and with Network true abstract sockets and the LAN stay reachable.
- **Agent-writable session folder:** host writes into it use CreateTemp+rename or `O_NOFOLLOW`;
  Save-to-folder and the zip download open files through `os.Root` at both ends; a worktree `.git` must
  point into its repo's `worktrees/` (with `commondir` leading back) before Diff, Accept or Prune run
  git; Discard removes worktrees only inside `DATA/worktrees`.
- **Built-in agent config:** the session manager writes `DATA/sessions/<id>/agent-config.json` (0600;
  agent, context and `files.upload_dir`, no token) at every launch and sets `CERBERUS_AGENT_CONFIG`;
  the agent reads it instead of `config.Load`, which would otherwise make a default config inside the
  hidden config folder and lose the owner's policy.
- **Policy** (stricter than the spec): `$(…)`, backticks, process substitution, brace expansion, a command
  name from a variable or glob, a pipe into an interpreter reading stdin, and unparsable lines are asked
  even in auto mode. The fork bomb is caught for any function name. A deny match whose positional
  argument is a variable or glob (`rm -rf $X`) is asked, not denied. Allow applies only when every
  command (nested ones too) matches and none runs under sudo, doas, pkexec, run0, systemd-run, flock or
  watch. Known limits are in the file doc and `TestPolicyKnownLimits`.
- **Feed:** a new kind `warning` (sandbox fallback), shown like a failure.
- **Web:** the Security view (`#/security`; More sheet on phones, the About popup on desktop) has
  devices with a two-press revoke, pairing with a QR, rotation behind a typed `ROTATE`, the doctor and the
  audit log with time and actor filters and paging. Monitor has a doctor card (counts, details dialog),
  loaded on mount and when the tab becomes visible; nothing polls. It moves into Settings in phase 4.
- **Open:** the agent can edit its own `meta.json`, and Accept trusts its `Repo` field (the `.git` check
  still binds the worktree to that repo); inbox delivery runs `git rev-parse` without the `.git` check
  (rev-parse runs no fsmonitor).


---

# Startup and configuration specification (day phase 4)

Starting must be one command with sensible results. Every knob must still be reachable, validated and explained.

## Config schema (`internal/config/schema.go`, new)

- A schema generated by reflection from the Config struct, plus a hand-written metadata table keyed by JSON path
  (`server.listen`, `engines.idle_ttl_sec`, …), with: label, help (one or two sentences), group (Server, Models,
  Gateway & spend, Sessions, Agents, Safety, Monitor & energy, Voice, Notifications, Advanced), level
  (`basic` | `advanced`), kind (string, int, float, bool, enum[...], duration-seconds, path, list, map, secret-ref),
  min/max, restart (true when the change needs a daemon restart), and sensitive (never returned; the token).
- `func Schema() []Field` and `func (c *Config) Get(path string) (any, error)`, `Set(path string, value any) error`
  (type-checked, validated), `Validate() []Problem` with `Problem{Path, Message string; Level string /* error|warn */}`.
- Validation covers: addresses parse, ports in range, durations ≥ 0, enums, referenced agents/providers/secrets
  exist, prices non-negative, tariff tiers ascending, paths exist (warn), and duplicate names.
- Every metadata entry must exist for every field (a test fails on any field without metadata, so new fields
  can't ship undocumented).

## CLI

- `cerberus init`: an interactive first run (reads answers from stdin; `--yes` takes all defaults; `--json` for
  scripts). It detects LM Studio / Ollama / llama-server and lists models, the GPU, installed agents (claude,
  opencode, aider), Tailscale (IP and whether `tailscale cert` works), the Atrium notify script, voice backends,
  bwrap, and RAPL readability.
  - It asks at most 6 questions: default model, phone access (none / LAN / Tailscale), HTTPS cert (if Tailscale),
    ntfy topic (optional), monthly household kWh (optional), and install the service now (yes/no).
  - It writes the config, prints a summary, the pairing URL (and QR when phase 3's encoder exists) and the doctor result.
- `cerberus config get PATH`, `config set PATH VALUE`, `config edit` ($EDITOR, then validate; re-open on error or
  keep the old file), `config validate`, `config schema [--json]`, `config path`, `config reset PATH`.
- `cerberus export [--secrets-names]` / `import FILE`: profile bundles (agents, repos, providers without keys,
  prices, budgets, tariff, monitor, limits) as JSON with a version field. Import shows a diff and asks first
  (`--yes`). Secrets are never exported, only their names.

## API

- `GET /api/config` → `{config (sensitive fields removed), problems, restart_pending}`, `GET /api/config/schema`,
  `PATCH /api/config {"set": {"path": value, ...}}` (validated atomically: all or nothing; returns problems), and
  `POST /api/config/restart` (graceful self-restart: re-exec the binary with the same args after a clean shutdown,
  supervised by systemd's Restart, or re-exec in place when not under systemd). Writes are audited.
- Changes that don't need a restart apply live (engines TTL, guard, budgets, prices, tariff, notify, monitor
  thresholds, agent profiles, repos), through a `Reload(cfg)` hook on each component that supports it. The rest
  mark `restart_pending`.

## Web: Settings and first run

- **Settings view** (in the More sheet on phones, a navbar tab "Settings" on desktop):
  - group navigation on the left (desktop) or as a list (phone); per group, Basic fields first, with an
    "Advanced" toggle;
  - fields rendered from the schema with help text, units, inline validation from PATCH problems, a "needs
    restart" badge, and a sticky Save/Discard bar showing the number of changes;
  - a restart button when pending;
  - search across all settings by label or help;
  - Security subsection: devices (from phase 3) with revoke, rotate token, the doctor results, and the audit log viewer.
- **First run:** when the config was just created (no `init` done: `Server.InitDone` is false, new field), the UI
  shows a 4-step welcome (model, phone access, notifications, electricity), then marks InitDone. It can be skipped.
- Agent profiles, repos, providers, budgets and tariff tiers get list editors (add, remove, reorder) instead of raw JSON.

## Acceptance

- Schema metadata coverage test; validation table; Set/Get round trips; atomic PATCH; live reload of TTL, guard and
  budgets proven by behaviour; restart round trip under a test supervisor; init with scripted stdin on a fixture
  machine (fake binaries); export→import round trip without secrets; screenshots of Settings (basic and advanced),
  validation errors, the first-run wizard, and devices/doctor/audit at 1280 and 390.

## Startup and configuration: implementation notes (as built)

Deviations from and additions to the specification above.

- **Schema:** `Field` also carries `format` (address, url, size, command, glob, email), `read_only`,
  `required`, `default` and `items` (the item schema of lists and maps). Kinds add `time` (read-only,
  `server.token_created`). Groups add **Files** (11 in all). Schema paths use `[]` for list items and
  `.*` for map values (`agents[].command`, `prices.*.in`); concrete paths use `[n]` and `.key` or
  `["key"]` (`prices["qwen2.5"].in`). The token and the whole `server` object are refused by path
  (`ErrSensitive`); `server.token_created` is `ErrReadOnly`.
- **Config API:** `Validate()` plus `ValidateWith(ValidateOptions{SecretExists})` for vault references;
  `SetAll(map)` applies all or nothing and refuses only changes that add a new error (errors already
  elsewhere do not block an unrelated edit); `Reset`, `Redacted`, `Diff`, `NeedsRestart`, `Export`,
  `Import` (returns the diff and an apply func). Missing paths warn, except default values (including a
  built-in list entry such as the default subscription's folder).
- **Migration:** a config file without `server.init_done` loads as done (existing installs see no
  welcome); a newly created one starts false.
- **Live model:** each PATCH clones the live config, applies `SetAllWith`, keeps the current token,
  saves, swaps the clone in and hands the same immutable pointer to every `Reload(cfg)` hook (engine
  manager, gateway, notifier, system monitor, energy, alerts, session manager, files store). Components
  read settings through `conf()`; the startup config only changes for token rotation (PATCH and rotation
  share one lock, so a save never writes an old token back). `restart_pending` is `Diff(boot, live)`
  filtered to schema-restart paths and paths no hook covers; reverting a change clears it. Still
  pending until their packages get hooks: `engines.load_timeout_sec`, `subscriptions`, `voice.*`.
- **PATCH replies:** 200 `ConfigView {config, problems, restart_pending, pending, restartable,
  changed[{path, live, restart}]}`; 422 `{error, problems}` on new validation errors; 400 for an unknown
  path, a wrong type, a sensitive or read-only setting, or an empty set; 409 when the config has no file or the file was edited into invalid JSON.
  Owners (admin or device) only. The audit note lists the changed paths with (live) or (restart).
- **Restart:** `POST /api/config/restart` answers 202 (503 without a hook) and restarts after 200 ms:
  a clean shutdown and a `serve.restart` audit event, then under the systemd unit (it sets
  `CERBERUS_SUPERVISOR=systemd`, and `INVOCATION_ID` is set; `INVOCATION_ID` alone is inherited by anything
  started from a terminal that runs as a unit, so it is not enough) exit 75 (the unit has
  `RestartForceExitStatus=75`), elsewhere on unix `syscall.Exec` of itself (same pid); on Windows exit 75.
  The restart is refused with 409 `{error, problems}` (the daemon keeps running) when the saved config
  would not start: it does not parse, a listen address fails bind safety, or a new primary listen address
  cannot be bound.
- **Listen safety in validation:** a public IP in `server.listen` / `extra_listen` without
  `server.allow_public` is a validation error (PATCH 422), the rule serve applies at start; a wildcard
  bind, or a public address with `allow_public`, is a warning. Host names are judged when the daemon binds.
- **Edits made outside the daemon:** the daemon remembers config.json as it last read or wrote it. A PATCH
  that finds the file changed (a hand edit, or the CLI writing the file because the daemon did not answer)
  is made on the file's contents, so that edit is kept and applied with it; a file that no longer parses
  refuses the PATCH with 409. A CLI request that times out is an error, not "daemon down": the file is
  only written when nothing answers.
- **Existing errors:** an edit is refused only for errors it brings. Errors are compared without list
  indices and counted, so deleting or reordering items in front of an item that was already invalid is
  allowed.
- **CLI extras:** `cerberus config restart`; `config schema --basic WORD`; `export --sections -o`. With the
  daemon running, `config set/reset/edit`, `import` and `init` go through PATCH, otherwise they write the
  file and audit as `cli` (`config.set`, `config.reset`, `config.edit`, `config.import`, `config.init`).
  A daemon on the configured address that refuses the file's token is treated as another install: the
  file is written instead.
- **init:** the install question is asked on Linux only and defaults to no. Phone access defaults to
  Tailscale when an address is found (in the interactive prompts: `--yes` and `--json` deliberately keep
  phone access `none` and HTTPS off, so an unattended run never opens the daemon to the network or points
  it at a certificate that does not exist yet; everything else takes the prompt defaults); Tailscale sets `extra_listen`, `allowed_hosts` and `public_url`;
  HTTPS sets `tls_cert`/`tls_key` under the config folder and prints the `tailscale cert` command
  (never run). An empty `notify.command` gets the desktop notify script when present. `--json` implies
  `--yes`.
- **Bundles:** version 1, kind `cerberus-profile`; sections agents (+ default agent), repos (+ repo
  roots), providers, prices, budgets, tariff, monitor, limits (guard, sessions, spawn), sandbox, policy,
  files. Credential-looking agent env entries and provider headers are always dropped and listed in
  `omitted`. Import merges lists by name (budgets by provider and period), keeps local secret names and
  is refused if it would add errors.
- **Web:** Settings at `#/settings`, `#/settings/<group>` and `#/settings/security` (the phase 3 panel,
  embedded); `#/security` redirects there and the More sheet entry is now Settings. The draft lives at
  module level so it survives moving between groups; the Advanced switch is remembered per browser.
  Nothing polls: data loads on mount and when the tab becomes visible with no unsaved edits. After a
  restart the page checks `/api/status` (uptime) with backoff up to 90 s, then reloads. The welcome
  sends one PATCH with only what changed, then `POST /api/config/init-done`.


---

# Chat specification (day phase 5): LLM chat from Atrium

A web "Chat" view that brings over the Atrium desktop LLM chat (llm-chat.py + llm_chat_engine.py + the mascots),
so the same experience works from any browser, a phone included. Reference inventory:
`~/.claude/projects/-home-carino-Github/cerberus-day/atrium-chat-inventory.md`. Source:
`~/Github/Atrium/home/.config/hypr/scripts/`. Atrium is the owner's own public project, so porting its code is fine.
Keep its wording ("LLM chat") and vendor neutrality.

## Server: chats (`internal/chat`, new)

- A chat is a conversation run **server-side** (tools execute on the host), streamed to the browser.
  Storage: `DATA/chats/<id>.json` (0600, atomic) in **Atrium's native session shape**
  (`{id,title,cwd,model,created,updated,messages}` with OpenAI messages; images stored as "[image]"), with id
  `chat-YYYYmmdd-HHMMSS-xxxxxx`. **Import**: list and open Atrium's own native sessions from
  `$XDG_STATE_HOME/hypr/llm-chat/sessions/` read-only. "Continue here" copies one into Cerberus.
- **Backends:**
  - **native**: the gateway (any local or API model) using the built-in agent's tool loop, refactored into a
    library: `agent.Run` with an event callback instead of terminal output. The events are Atrium's wire protocol:
    `start{step}`, `text`, `reasoning`, `tool{id,name,canon,input}`, `tool_done{id,output,is_error}`, `ask{req}`,
    `no_tools{why}`, `usage{prompt,completion}`, `done{ok,stopped,error,steps,seconds,usage}`.
  - **claude**: only when the `claude` CLI exists. One `claude -p --output-format stream-json --verbose
    --include-partial-messages --input-format stream-json --permission-prompt-tool stdio --permission-mode MODE
    [--resume ID] [--model M]` process per message, with the control protocol (can_use_tool → ask card → response)
    as in llm-chat.py (`argv` 2880, `can_use_tool` 672, `control_response` 692). Its sessions are read from
    ~/.claude/projects for the recent list (title from `ai-title`).
  - Switching backend starts a new conversation (as Atrium does).
- **Modes:** Ask me / Auto (read-only safe commands, `SAFE_COMMANDS`/`SAFE_GIT` port) / Accept edits /
  Plan (read-only tools, plan instruction).
- **Permission cards:**
  - Allow once / Allow for this session / Deny with an optional note; session rules (exact, `prefix:*`, `dir:`,
    `domain:`, tool) auto-answer covered cards.
  - Two-press confirm for run_command (400 ms – 5 s window). Buttons are armed after 600 ms.
- **Working directory:** chosen per chat with the existing folder picker (phase "workspaces"), default the user's
  home. Deny paths and the phase-3 command policy apply. Reads outside the folder ask (an in-page version of
  Atrium's Folder access).
- **Limits** as Atrium: 12 tool steps per message, results clipped to 12 000 chars, run_command 120 s default
  (600 max). Images scaled to ≤ 1568 px (PNG, JPEG q88 over 3.5 MB) client-side before upload.
- **Queueing:** a message sent while busy queues. Stop cancels the stream and the command's process group and
  denies open cards. A tab closing does NOT stop a running chat (it keeps running server-side). Notifications
  arrive through the existing notifier when the chat finishes or waits and no browser is watching.
- **Context status:** tokens used / context length (from model info), elapsed, cost for API models (phase 2 prices).
- **API:** `GET/POST /api/chats`, `GET /api/chats/{id}` (messages + state), `DELETE /api/chats/{id}`,
  `POST /api/chats/{id}/messages {text, images[], model?, mode?}`, `GET /api/chats/{id}/stream` (SSE of the events
  above; replay since an event index), `POST /api/chats/{id}/answer {req, decision: once|session|deny, note}`,
  `POST /api/chats/{id}/stop`, `GET /api/chats/search?q=`, `GET /api/chats/atrium` (importable),
  `POST /api/chats/atrium/{id}/continue`. Quick answer: `POST /api/quick {text, model?}` → SSE plain stream
  (Atrium's `??`: ≤ 3 short sentences, max_tokens 400, temperature 0.3, no tools).

## Web: Chat view

- Navbar tab "Chat" (phones: in the bottom bar replacing Wall if needed; at most 4 bottom tabs plus the More sheet).
- **Layout** as Atrium's:
  - a stage (mascot + speech bubble) on top on phones and at a right-hand column ≥ 1100 px, plus the transcript
    and an input strip with attachment pills, Send/Queue, Stop and the status line;
  - the stage can be collapsed to a corner card (click to move corners), and the state is remembered per browser.
- **Rendering:**
  - a safe markdown renderer built with DOM nodes, never innerHTML with model text: h1–h3, bold, italic, strike,
    code spans, http(s) links (rel noopener, target _blank), bullets, numbered lists, task lists, quotes, rules,
    pipe tables with sideways scroll, fenced code blocks with a language label and copy button;
  - a copy-raw button per reply, a thinking fold ("thinking · N lines"), tool chips with spinner → ✓/✗ and an output
    excerpt (auto-open on error), notes, error cards, and the end-of-run line ("✓ done · 3 steps · 4.2 s · model").
- **Model picker** (local / server / API providers / Claude when present) with loaded marks, defaults, "unload",
  and "applies to the next message" notes; the mode menu; session rules listed with "Forget them"; and a
  recent-chats sheet (Cerberus + Claude + Atrium importable) with two-press delete, search, and continue.
- **Input:** Enter sends, Shift+Enter newline, Up/Down history, Esc stops; paste and drop images (reuse phase 2
  drop helpers); the mic button (phase 2 voice) inserts text; Alt+A/S/D answer the oldest card; Ctrl+F finds in
  the transcript; Ctrl+N new chat; Ctrl+O recent; Ctrl+M model; Ctrl+J mascot menu; Ctrl+K corner card.
- **Theme:** Cerberus' Carino palette by default. The mascot theme is chosen separately (all 11).

## Mascots (`web/dist/assets/js/mascots/`, a JS port)

- Port `mascot_common.py` (Canvas, LCG, blink, emotes, prop_kind), `mascot_mood.py` (moods, scenes, emotes,
  reactions, the sentiment scorer, DEFAULT_LINES) and **all 11 theme modules** (moe, matrix, win95, aero, cyberpunk,
  lofi, luna, orbit, seasons, orchard, orchard-dark) plus the `_StandIn` kaomoji, to ES modules.
- They render to a `<pre>`-like grid of coloured spans (DOM nodes or one canvas per frame: use DOM `<span>`
  runs merged per colour for crispness and screen readers get `aria-hidden` plus the bubble text as the label).
- States: idle, thinking, tool(name→prop), writing, done, error, with done/error → idle after 15 s. Moods, scenes
  and reactions are driven by chat events (prompt, reply, tool_start/ok/error with the tests/commit regexes,
  result, stopped, permission, model, poke on click, pet on double click, poke_spam). Surroundings: time of day only
  (the desktop weather/calendar sources are dropped), plus `prefers-reduced-motion` (tick ÷ 3, steady emote).
- Timing: 10 ticks/s while active, 2.5/s after 10 s quiet, stopped after 120 s and whenever the tab is hidden.
- Grid fills the stage (min 48×14, natural 160×24, max 480×120 cells) with font scaling 0.6–2.0.
- Bubbles: per-state, tool, mood and topic lines per theme (moe bilingual), with the priority override → say → bubble.
- **Golden tests:** for each theme, generate reference frames with
  `python3 ~/Github/Atrium/home/.config/hypr/scripts/mascots.py --frame STATE TOOL TICK COLS ROWS` (read the
  script for exact flags and theme selection), commit them as fixtures under `web/test/fixtures/mascots/`, and have
  node tests assert the JS output matches character-for-character (and colours where the frame output includes
  them) for a set of states, ticks, tools, moods and sizes.
- The notification sprites (`mascot_icons.py`, 16×16 per theme): port too and use them as the PWA/notification
  icon per theme (rendered to a canvas → data URL).

## Acceptance

- Engine refactor keeps every existing agent test passing; chat e2e with a fake OpenAI server (streaming text,
  reasoning, a tool call needing approval → once/session/deny paths, queue, stop, persistence, reload replay);
  claude backend against a fake `claude` script speaking stream-json and the control protocol; Atrium session
  import; quick answer.
- Mascot golden tests for all 11 themes.
- Screenshots: chat desktop (stage at right) and phone (stage on top), a permission card, the corner card, the
  model picker, recent chats, and each mascot theme in at least two states.

## Chat: implementation notes (as built)

- **Engine:** `agent.Run(ctx, RunOptions, emit)` returns `Result{Messages, System, Done, Err}`; the done event is
  always emitted and also carries `total` (summed usage), `calls` and `cost` (when a price is known). Extra events:
  `note{text}` (e.g. the no-images fallback). A fifth mode `bypass` exists only for `cerberus agent --approve auto`;
  chats refuse it. A policy "always ask" card is `Forced`: session rules never answer it. Reads of protected paths
  are refused without a card. Every existing agent test passes unchanged.
- **Chat events** (SSE, each with `i`, the event index, and SSE `id:`/`event:` lines): the engine's plus `user`,
  `queued`, `dropped`, `answered{req, decision, by: user|rule|stop|end|cancel, note?, rule?}`, `state{busy, state,
  queue}`, `status`, `rules`, `switched{to, model}`, `error`. A card that a session rule covers is answered without
  an `ask` event. Logs are capped at 5000 events per chat and live in memory (a restart keeps the files, not the
  logs); a subscriber 512 events behind is dropped and reconnects from its last index; idle streams get a
  keep-alive every 25 s.
- **Behaviour:** Stop also drops the queue. Deleting a running chat is 409. Session rules are per chat, in memory.
  Continuing the same desktop or `claude` session twice returns the same chat (`source` field). After a failure
  with nothing returned the user message is dropped. Chat files keep the desktop keys plus optional `backend`,
  `mode`, `claude_session`, `source`, `cost_usd`, `context_used`. Claude models are `claude` / `claude:<model>`.
  Security: the "own folder" exemption from the protected paths applies only when the working folder is inside a
  protected folder, so a chat rooted at home cannot read chat files or session records.
- **API additions and deviations:** `PATCH /api/chats/{id} {cwd?, model?, mode?}` (the web uses it for the folder;
  model and mode go with the next message); `GET|DELETE /api/chats/{id}/rules` (also `/api/chats/rules?chat=ID`);
  `GET /api/chats/models` (`{models, default, quick_model, default_mode, modes, claude: {available, models}}`;
  `/api/models` is unchanged); `GET /api/chats/claude` and `POST /api/chats/claude/{id}/continue`; one desktop
  session opens read-only at `GET /api/chats/atrium?id=ID` (the path form clashes with `{id}/stream` in ServeMux).
  The stream takes `?from=N` and honours `Last-Event-ID`. Status codes: 201 create, 404 unknown or malformed id,
  409 busy or already answered, 403 while `chat.enabled` is off (quick answer included), 503 without a manager,
  413 over 64 MiB. Chat routes and `/api/quick` count toward the per-actor stream cap and end when the credential
  is revoked. Audit records name model, mode, image count, decision and rule, never message text.
- **Review fixes (as built):**
  - *Streams:* every chat stream starts with `hello {epoch, base, next}` (no SSE `id:`). The epoch is random per
    daemon process; `GET /api/chats/{id}` also returns `epoch`, `base` (the oldest event the log holds) and
    `run_from` (the running message's `user` event, -1 when idle). The page reloads the chat when the epoch
    changes, the log is behind it (a restart) or `base` is above the index it asked for (a gap); an index above
    `next` replays from `base`. A busy chat replays from `run_from` when the log still holds it, else it shows the
    stored messages, open cards and queue and listens from `next`. Queued messages from before the running one
    are drawn above its bubble. "N waiting" follows the open cards live.
  - *Queue and stop:* Stop empties the queue, so a message sent while a stopped run is still ending queues and
    runs after it. A queued message that cannot start is reported (`error`) and the next one tried.
    `Manager.Stopping()` runs before the listeners shut down: running chats end as stopped (no "failed"
    notification), nothing queued starts, and new messages get 503 (`ErrClosing`).
  - *claude backend:* a claude process that exits with a card open denies it (`by: end`) instead of waiting.
    Before any card or session rule, `can_use_tool` goes through the same guards as native runs: deny_paths, the
    config folder and the data folders (sessions, worktrees, chats) are refused with no card; the command
    policy's deny is refused, its ask becomes a Forced card, and so does a command that names a protected folder.
    The CLI also gets `--settings {"permissions":{"deny":[Read(...), Edit(...)]}}` for those folders and
    deny_paths, which covers the reads and edits it makes without asking. Remaining gap: commands the CLI runs
    without asking (its own read-only Bash allowances) are not seen by Cerberus, and Accept edits lets it edit
    anything else inside its working folder (home by default). Pick a narrower folder for claude chats.
  - *Session rules:* a relative path is resolved against the chat's folder; a file tool never becomes a
    tool-wide rule (no derivable folder → the answer counts once).
  - *Memory:* at most 16 idle chats stay loaded (least recently used first out); busy, watched or waiting chats
    and chats with session rules or a queue stay. An evicted chat loads from disk again (images as `[image]`).
  - *Continue* is serialized, so two at once return the same chat. Agent sessions also refuse `DATA/chats`.
  - *Web:* Alt+N is the new-chat key (browsers keep Ctrl+N; it still works in an installed PWA window). Desktop
    sessions open read-only from the recent sheet, with "Continue here" in place of the input. With a phone
    keyboard open (`body[data-kb]`, from the visual viewport), the stage, the header's tool and quick rows and
    the bottom tabs fold away; the stage height follows `--app-h`, not `vh`. The phone corner card is at least
    264 × 180 px so the 48×14 grid fits at scale 0.6.
- **Quick answer:** `POST /api/quick` errors before the first word come back as JSON (400 bad input, 502 model
  unreachable); then SSE `text{text}`, `done{ok, model}`, `error{error}`.
- **Daemon wiring:** chats live in `DATA/chats`; native chats call this daemon's own `/v1` over loopback (a wildcard
  listen address becomes 127.0.0.1 or ::1) with the current admin token, so rotation applies. Context lengths come
  from a non-blocking 30 s cache of the engine's model list. `chat.*` settings apply live. The manager closes after
  the listeners stop.
- **Config (add-only):** `chat {enabled, quick_model, default_mode, max_steps, result_clip, command_timeout_s,
  atrium_import}`, shown in the Agents settings group; `chat.quick_model` may not be a `claude:` model.
- **Web:** `#/chat`, `#/chat/new`, `#/chat/<id>`; `g c` goes there. The phone bottom bar is Sessions, Chat, Monitor,
  More (Wall moved into More). A busy chat renders the messages before the last user message and rebuilds the
  running one from the event log (`replayFrom`); reconnects resume at the last index. A new chat is created on its
  first send. The model picker groups "Local · backend", "API · provider" and "Claude"; there is no separate
  server group (ModelInfo has no flag for it). History names tools as the live events do (`CANON`). The cwd label
  keeps its slashes in place under `direction: rtl` with LRM marks. The corner card's bubble is one line. Browser
  keys: `cerberus.chat.stage`, `cerberus.chat.last`, `cerberus.chat.history`, `cerberus.mascot`. The mascot
  notification icons (`icons.js`) are ported but not wired, as the page sends no notifications of its own.
- **Mascots:** `mascots/index.js` registers the 11 themes and loads them lazily (unknown or failing → moe → the
  kaomoji stand-in). Each theme exports `meta {id, name, panel, moods}`, `lines`, `spans`, `render`, `bubble`, `say`,
  `emoteColours`. Clock-drawing themes (win95, luna, orbit, seasons) expose `setClock`; seasons also `setSeason` and
  `setHemisphere` (north by default). Surroundings are the time of day only. Golden fixtures under
  `web/test/fixtures/mascots/` come from the desktop `mascots.py --frame` plus a read-only markup dump for colours;
  every theme matches text, bubble, colour runs and head anchor exactly.


---

# Extras specification (day phase 6)

Smaller features that round Cerberus off. Config additions are add-only and each new field needs schema
metadata (the coverage test enforces it).

## Notification centre

- Every notification Cerberus sends (session finished, waiting, failed; chat finished or waiting; budget, alert,
  doctor fail; downloads done) is also stored in an in-app inbox: `DATA/inbox.jsonl` (0600, ring of 1000, rotated)
  with `{ID, Time, Kind, Level (info|warn|crit), Title, Body, Link (in-app route), Read}`.
- `GET /api/inbox?unread=1&limit=`, `POST /api/inbox/read {ids|all}`, `DELETE /api/inbox/{id}`, and inbox items on
  the existing SSE event stream (no new stream).
- Web: a bell in the navbar actions block with an unread badge; a panel (desktop) or sheet (phone) listing items
  newest first, grouped by day, tap → route, "Mark all read". Desktop browser notifications only when the user
  opts in (Notification permission requested from a click), and only while a tab is hidden.

## PWA and offline shell

- `manifest.webmanifest` (name "Cerberus", short_name, theme/background from Carino tokens, display standalone,
  start_url "/", scope "/", icons 192/512 + maskable, rendered at build time into web/dist as PNGs from the Carino
  mark; never hand-drawn: use the existing Cerberus/Carino mark assets in web/dist or the hub gold C).
- `sw.js`: caches the app shell (HTML, JS, CSS, fonts, icons) with a versioned cache name derived from a build hash;
  **never caches /api, /v1 or SSE**; offline navigation shows the shell with an "offline: the host is unreachable"
  banner and retry. Update flow: a new version shows "Reload to update".
- Served with correct content types and `Service-Worker-Allowed` only for "/". Works over the Tailscale HTTPS
  listener (service workers need a secure context; document that plain-HTTP LAN can't install).

## Session recording and replay

- Each session run also writes `DATA/sessions/<id>/runs/<attempt>.cast` (asciicast v2: header with width, height,
  timestamp, title; `[t, "o", data]` events; input events never recorded), capped by `Sessions.RecordMaxMiB`
  (new, default 16; 0 = off) after which recording stops with a final marker. Masked through vault values like the
  scrollback. Pruned with the session.
- `GET /api/sessions/{id}/runs` and `GET /api/sessions/{id}/runs/{attempt}.cast`.
- Web: a "Replay" tab in the session view: an xterm player with play/pause, speed (1×, 2×, 4×, 16×), idle-gap
  skipping (cap pauses at 2 s), a scrubber with activity marks, and copy-link to a time.

## Model download manager

- Download local models without the CLI: LM Studio (`lms get <query> --yes` when lms exists, progress parsed
  from its output), Ollama (`POST /api/pull` streaming), and plain GGUF from a Hugging Face URL into the llama.cpp
  models dir (resumable HTTP Range download to `.part`, SHA-256 when the repo publishes it, refuses when free disk
  < size + 2 GiB).
- One download at a time per engine, queue, cancel, progress (bytes, rate, ETA), inbox item when done.
  **Never loads a model** after download.
- `GET/POST /api/downloads`, `DELETE /api/downloads/{id}`, progress on the event stream; `cerberus pull <ref>`.
- Web: a "Get models" sheet in Models with a search box (LM Studio catalogue via `lms get --json` if available,
  Ollama library names typed, HF URL pasted), queue with progress and cancel.

## Leaderboard

- From existing usage and energy data: per period (today, 7 d, 30 d, month), rank sessions, agents, models and repos
  by cost (USD and MXN through the existing FX handling if present, else USD), tokens, energy (kWh, MXN marginal)
  and wall time. `GET /api/leaderboard?period=&by=session|agent|model|repo&metric=cost|tokens|energy|time`.
- Web: a "Leaderboard" section in Usage with a by/metric switch and bars (accessible table under the bars).

## Product landing page (cerberus.carino.systems)

- The repo root is the GitHub Pages site (CNAME `cerberus.carino.systems`). Add a root `index.html` with assets
  under `site/` as a fleet **product** page: read the memory notes carino-page-types, carino-navbar-convention,
  carino-navbar-always-visible, carino-no-footers, carino-fonts-convention, carino-marks-convention,
  fleet-landing-hero-pattern, carino-theme-persistence and carino-branding-reference, and copy the shared files
  (carino-navbar.js, carino-clock.js, carino-theme.*, fonts) from ~/Github/Branding (read-only source).
- Content: what Cerberus is (local models + one OpenAI-compatible gateway + coding agents in worktrees steered from
  a phone), the three parts, energy focus, safety story (link docs/SECURITY.md on GitHub), install steps
  (`make build`, `cerberus init`, `cerberus install-service`), screenshots (compressed, from the phase shots), and
  a short FAQ (vs Orca, subscriptions only through official CLIs, Linux first). Vendor neutral wording ("LLM").
- No analytics and no external requests; no footer (About in `[data-carino-about]`).
- `docs/SITE.md`: the proposed baseline `sites.json` entry (type "product") and hub tools.json entry as a note for
  the owner. **Do not edit any other repository.**
- The landing page must not be served by the daemon (web/dist stays the app).

## Acceptance

- Unit and e2e for the inbox (ring, rotation, read state, SSE delivery), sw.js never caching /api (test by parsing
  its routing table and a headless check), cast writer (format, cap, masking, no input), replay player helpers,
  download manager against fake lms/ollama/HTTP servers (resume, checksum, disk-space refusal, cancel), leaderboard
  aggregation table.
- Screenshots: bell panel desktop and sheet phone, offline banner, replay tab, Get models sheet with a running
  download, leaderboard, landing page at 1280 and 390.

## Extras: implementation notes (as built)

- **Inbox** (`internal/inbox`): `Open(path, keep)` keeps a ring in memory and appends JSON lines; when the file
  reaches twice the ring it is rewritten atomically (no `.1` file, so deleted items do not survive on disk).
  Item JSON uses the Go field names (`ID, Time, Kind, Level, Title, Body, Link, Read`). `notify.Send` runs the
  rate limit, then stores the message even with `notify.disabled`; `notify.Classify` fills Kind/Level/Link
  when the sender left them empty (alerts send `alert` → `#/monitor`, budgets `budget` → `#/usage`, downloads
  `download` → `#/models`). No doctor notification exists yet, so no `doctor` items are produced.
- **Event stream:** `/api/events` carries `session`, `inbox`, `download` and `resync` (`{"what":"inbox"|
  "downloads"}` after a dropped subscription; the client refetches that list). The UI keeps this one stream open
  while hidden only when desktop alerts are on and permitted.
- **PWA:** `/manifest.webmanifest` and `/sw.js` are public (browsers fetch the manifest without credentials),
  served with ETags, `no-cache`, and `Service-Worker-Allowed: /` on sw.js. `python3 web/tools/build_sw.py`
  regenerates sw.js (BUILD hash and SHELL list) after any change under web/dist; `TestServiceWorkerBuild` fails
  while it is stale. Icons come from `python3 web/tools/make_icons.py` (rsvg-convert + Pillow) rendering
  `web/dist/assets/icons/cerberus.svg`, a copy of the Branding mark. Plain-HTTP LAN cannot install the app.
- **Recording** (`internal/session/cast.go`): the cast gets the same masked bytes as the scrollback, is written
  through `os.Root` with `O_EXCL|O_NOFOLLOW` at 0600, and stops with an `o` notice plus an `m` marker at the
  cap. Resize events are not recorded; notes written after the process exits (check output) are not in the cast.
  Agents read their own casts at `/api/agent/runs` and `/api/agent/runs/{N}.cast`.
- **Downloads** (`internal/downloads`): queues per engine (`lmstudio`, `ollama`, `llamacpp`); progress events
  are throttled to about 2/s. A user cancel deletes `.part`; a failure or shutdown keeps it for resume. Queued
  and finished items live in memory only: a daemon restart forgets them. HF URLs must be https huggingface.co /
  hf.co, redirects only to https `*.huggingface.co` / `*.hf.co`; an existing file is never overwritten. The disk
  check is skipped on Windows. `lms get` has no machine-readable catalogue search, so the Get models sheet has
  no search list: a typed LM Studio name goes straight to `lms get <name> --yes`.
- **Leaderboard** (`usage.Leaderboard`, `energy.(*Energy).Leaderboard`): cost is USD only (there is no FX
  source); energy cost uses the tariff currency at the marginal price. Gateway traffic with no session is one
  "(direct API)" row (split per client when grouped by agent). A session's period energy is its lifetime wall
  energy times the share of its life inside the period, or its requests' GPU energy when that is higher.
- **CLI:** `cerberus pull [--engine E] [--sha256 H] [--detach] [--cancel ID] <ref>` opens the event stream
  before it posts, so no progress is missed; Ctrl-C stops following (exit 130), not the download.
- **Landing page:** the repo root `index.html` with `site/` (see docs/SITE.md); web/dist stays the app and the
  daemon never serves the landing page.


---

# Hardening and documentation (day phase 7)

Six phases landed in one day. This pass looks at the product as a whole, from a fresh install.

## Fresh-install walkthrough

- On a temp HOME/XDG with fake engine and agent binaries on PATH: `make build`, `cerberus init --yes`, `cerberus serve`
  on a test port, `cerberus doctor`, then a tour of every view at 1280 and 390: Sessions (new task in each workspace
  mode, attachments), Wall, Chat (fake OpenAI server), Monitor, Usage (leaderboard), Models (Get models), Secrets,
  Settings (every group), Security, the bell, the More sheet, and the first-run welcome on a config without init_done.
- Fix every broken flow, console error, layout overflow at 360–1280 px, and inconsistency between views (button
  styles, empty states, wording) found on the way.

## Cross-cutting checks

- Idle cost: daemon idle RSS and CPU with every phase enabled (target as Phase 1: no polling while nobody watches;
  measure over 60 s with no browser open and with one hidden tab).
- Every EventSource and timer stops when the tab is hidden (grep and a headless check).
- Every state-changing route has auth, CSRF and audit; the agent scope reaches only what the spec allows (generate
  the route table from the mux and test it as a matrix).
- Every file Cerberus writes under DATA and the config dir is 0600/0700 and written atomically.

## Documentation

- README.md: what Cerberus is, install (`make build`, `cerberus init`, `cerberus install-service`), phone access,
  the feature list grouped by the three parts plus control center, files, safety, chat, extras; links to SCOPE,
  SECURITY, DESKTOP and SITE docs.
- docs/SCOPE.md: an "As built (day plan)" section listing the deviations each phase reported, so the spec matches
  the code.
- `cerberus help` and each subcommand's help text are complete and consistent.

## Acceptance

- The walkthrough screenshots, the idle measurement, the route matrix test, and a clean full gate.

---

# As built (day plan)

Where the code differs from the day-plan specifications above, phase by phase. Each phase's
"implementation notes (as built)" section has the detail; where a note there and this list disagree,
this list is newer.

## Monitor (day phase 0)

- The marginal-tariff result type is `bill.Margin` (Go forbids a type and a function both named
  `Marginal`); JSON is unchanged.
- `/api/monitor/series` returns a bare array of `{T, CPU, GPU, Rest, Wall}` in watts; `since` takes
  RFC 3339, a Go duration or `Nd`. Downtime counts as 0 W in hour and day points.
- `GET /api/monitor` is passive unless `?watch=1`; active sampling follows the stream, `cerberus top`
  and polls with a 60 s lease. With `monitor.enabled` false the process routes answer 404.
- Process signals are `term|kill|stop|cont`; renice only 0..19 and never lower. Protection is built
  from a fresh process-table read per call (compositors, their ancestors and session leaders, the
  daemon and its ancestors, desktop session wrappers). Command lines are masked with vault values.
- On phones Usage is a collapsible section inside Monitor (and, since phase 5, also in More).

## Control center (day phase 1)

- Extra feed kinds: `update` (rename, tags, pin, priority, limits, Save to folder) and, from phase 3,
  `warning`. Approve and reject are `state` items.
- Feed input is logged per line ("text ⏎", key names), masked first and clipped after (80 / 240
  characters). Input typed with the terminal's echo off reads "(hidden input) ⏎".
- Limits are per run: a retry, resume or follow-up starts the time and output limits fresh. A new
  memory cap applies from the next run.
- Spawn limits count open sessions only. A pending spawn survives a restart; stopping or killing it
  keeps it unapproved, so it can only be discarded.
- `cerberus_send` (`POST /api/agent/sessions/{id}/send`) only starts a new run of a finished or
  stopped child; a running child is 409. A parent cannot type into a child's terminal or answer its
  approvals.
- A spawned task, model or send text that starts with "-" is 400.
- The per-session agent token carries its session id (`cas-<id>.<HMAC>`), because `/v1` clients send
  no session header. Admin token or cookie on `/api/agent/*` is 403.
- Bulk replies are `{results, ok, skipped, error}`; more than 100 ids marks every id `error`. Bulk ops
  run detached from the request (30 min cap).
- The wall stream ends when any member's stream ends; the client reconnects. Backlogs are `out`
  events with `"backlog": true`.
- No user bus: a probe scope decides, and on failure scopes are off for the daemon's lifetime and the
  run restarts without one. `/api/status` guard gains `memory_enforced` and `session_memory_max`.
- `--mcp-config` is added only for `claude` profiles, only with spawning on, and only when the argv
  has none.

## File drop (day phase 2)

- Names keep only the last path part; Windows-refused characters become "_"; `.env` is stored as
  `env` but still gets 0600. NFC covers Latin-1 Supplement and Latin Extended-A single marks only.
- With `files.allow_exec`, only content starting with `#!` gets the exec bit.
- `max_total_mib` counts every byte ever uploaded to a session; deletes do not give it back.
  Concurrent uploads share one counter.
- The repo exclude line goes into the shared `info/exclude`, so new untracked `inbox/` files are also
  hidden in the main checkout.
- Staging ids are claimed before use; a failed Create puts every file back. Fan-out copies are real
  copies, never hard links.
- Recursive delete works only inside the inbox (anywhere in a staging folder) and refuses a symlink
  anywhere on the way.
- The built-in agent's inbox ledger is written only after a request succeeds. At most 4 images of
  5 MiB each per run go as image parts; a vision refusal retries without them.
- Reading a workspace (list, preview, download) creates no inbox and runs no git.
- PDFs are not CSP-sandboxed (the browser's viewer would not render); the preview iframe is
  `sandbox=""` with an "open in a new tab" link.
- Agent-scope files (`/api/agent/files`, `/files/raw`) exist; there are no MCP tools for them yet.
- Bridge: a receiver that accepts only an exact allow list never answers a self-hosted Cerberus
  origin; the sender times out after 30 s.

## Safety (day phase 3)

- The device store, QR encoder and doctor are subpackages of `internal/api`. Config adds
  `server.token_created` (add-only).
- Rate limit: 5 wrong credentials a minute from one address (an IPv6 /64) block that address's
  failing requests with 429 for 10 minutes. **Valid credentials still pass a block**, so a neighbour
  on loopback cannot lock out the owner, the CLI or agents. Cross-site `?token=` failures are refused
  but not counted.
- Host-check failures are 403; under a wildcard listen any IP literal is accepted. The address really
  bound is checked too, including retried extra addresses.
- The audit middleware skips `/v1/*` and `POST /api/usage/watch`. Terminal input is one record per
  line (or after 2 s idle, or 4 KiB); hidden input is "input (hidden, N bytes)".
- Rotation rewrites only `server.token` and `server.token_created` in the file, so hand edits
  survive. Up to 8 old tokens keep live sessions' `/v1` access until those sessions end.
- Devices can list and revoke but not pair. Pairing URLs prefer `public_url`, then the request's own
  address, then other non-loopback addresses.
- The doctor has 16 checks and never creates a config.
- bwrap adds `--unshare-pid`, `--chdir`, hides DATA, the config folder and `$XDG_RUNTIME_DIR`, and
  treats unknown sandbox words as bwrap. Inside it `.git` is read-only (commit on the host) and TUIs
  have no controlling tty.
- Host git calls disable hooks and fsmonitor; diffs use `--no-ext-diff --no-textconv`. A repo whose
  own git config runs commands is refused (LFS and named credential helpers allowed).
- The command policy is stricter than the spec: substitutions, variable command names, `source` of
  stdin, unparsable lines and similar are always asked. Default deny adds `systemctl
  reboot|poweroff|halt`, `poweroff`, `halt` and `git worktree add -b|-B`.
- The built-in agent reads `DATA/sessions/<id>/agent-config.json` (no token), not the config file.
- Open: the agent can edit its own `meta.json`, and Accept trusts its `Repo` field (the `.git` check
  still binds the worktree to that repo).

## Startup and configuration (day phase 4)

- The schema adds `format`, `read_only`, `required`, `default` and `items`; groups add **Files**.
  Schema paths use `[]` and `.*`; concrete paths `[n]` and `.key` or `["key"]`.
- The token and the whole `server` object are refused by path as one value; single `server.*`
  fields can be set. `server.token_created` is read-only.
- A config file without `server.init_done` loads as done, so existing installs see no welcome.
- An edit is refused only for validation errors it adds; existing errors elsewhere do not block it.
- A public IP listen address without `allow_public` is a validation error (422); a wildcard is a
  warning.
- A PATCH that finds config.json edited by hand builds on the file, keeping that edit; a file that
  no longer parses is 409. A CLI request that times out is an error, not "daemon down".
- Restart: 202, then exit 75 under the unit (`CERBERUS_SUPERVISOR=systemd` plus `INVOCATION_ID`),
  `exec` of itself elsewhere on unix, exit 75 on Windows. A saved config that would not start is
  refused with 409 and the daemon keeps running.
- Still "needs restart" for lack of reload hooks: `engines.load_timeout_sec`, `subscriptions`,
  `voice.*`.
- `cerberus init --yes` and `--json` keep phone access off and HTTPS off; the install question is
  Linux only and defaults to no.
- Import diffs print long values in full. Credential-looking agent env entries and provider headers
  are dropped from bundles and listed as `omitted`.
- Security moved into Settings (`#/settings/security`); `#/security` redirects there.

## LLM chat (day phase 5)

- `agent.Run` adds a `note` event and a fifth mode, `bypass`, only for `cerberus agent --approve
  auto`; chats refuse it. A policy "always ask" card is never answered by a session rule.
- Every chat stream opens with `hello {epoch, base, next}`. Event logs are in memory (5000 per chat);
  after a restart or a gap the page reloads the chat from its file.
- At most 16 idle chats stay loaded; busy, watched or waiting chats stay.
- Stop also empties the queue; a message sent after Stop runs after the stopped reply ends. During
  shutdown running chats end as stopped and new messages get 503.
- The `claude` backend: an exit with a card open denies it. Its tool requests pass the same path and
  policy guards as native runs, and it gets `--settings` deny rules for protected folders. Gap: the
  CLI's own unasked read-only commands are not seen by Cerberus. Pick a narrow folder for claude chats.
- Desktop chats open with `GET /api/chats/atrium?id=ID` (a path form would clash with `{id}/stream`).
- `chat.quick_model` may not be a `claude:` model. Agent sessions refuse `DATA/chats`.
- Web: Alt+N starts a new chat (browsers keep Ctrl+N). The phone bar is Sessions, Chat, Monitor,
  More; Wall moved into More. The model picker has no separate "server" group. Mascot notification
  icons are ported but not wired.

## Extras (day phase 6)

- No doctor notification exists yet, so the inbox has no `doctor` items. The inbox file is compacted
  in place (no `.1` file).
- `/api/events` adds `inbox`, `download` and `resync` events. The one stream stays open while the
  tab is hidden only when desktop alerts are on and permitted.
- The service worker answers navigations network-first but keeps serving its own cached page until
  "Reload to update", so page and modules stay one build. A headless test checks the caches.
- Recordings omit resize events and anything written after the process exits (check output).
- Downloads: queued and finished items are in memory only (a restart forgets them; `.part` files
  stay for resume). `sha256` is accepted only for Hugging Face GGUF (400 otherwise, and `pull` exits
  2). A bare name lets the server pick LM Studio or Ollama. There is no LM Studio catalogue search:
  `lms` has no machine-readable search.
- Leaderboard cost is USD only (no FX source); energy cost is in the tariff currency. A session's
  window is its first run start to its last run end (`session.Info.RunStarted`, `RunEnded`, add-only),
  so queue time and later accepts do not count. Gateway traffic without a session is one
  "(direct API)" row.
- The landing page is English only. The repo is AGPL-3.0-or-later (`LICENSE`), and the landing
  page and the web UI show it in the About window.

## Hardening and documentation (day phase 7)

- `cerberus help` groups the commands; `cerberus help COMMAND` equals `cerberus COMMAND -h`. Every
  subcommand's -h prints a "Usage: cerberus …" line, a short description and its flags, and exits 0
  (the texts live in `cmd/cerberus/help.go`).
- `cerberus agent -h` prints the same "Usage: cerberus agent [flags] [task]" heading (set in
  `internal/agent`).
- Route matrix (`internal/api/routes_matrix_test.go`): the route registration records every
  pattern it puts on the mux (`routeMux` wraps `http.ServeMux`); each pattern must be in an
  explicit table (public, owner, agent, gateway, preview). Every pattern and method is checked for
  401 without a credential, 403 for a cookie write without `X-Cerberus`, admission of a device
  bearer, and the agent scope (session token only on `/api/agent/*` and `/v1`, the shared agent
  token only on `/v1`). Every state-changing route writes exactly one audit record; `/v1` (recorded
  in usage) and `/api/usage/watch` are exempt. No route needed a fix.
- File modes (`cmd/cerberus` `TestServeFileModes`, umask 0): every file under DATA and the config
  dir is 0600 and every folder 0700. Staged uploads (`DATA/uploads/<id>/`) and session attach
  folders are "held" folders: all files 0600 (0700 when executable and `allow_exec`), and which
  names are secret is kept in a `.cerberus-upload-private` list inside the folder (skipped by
  listings, deliveries and the sweep). Delivery into a workspace still uses the normal modes (0644,
  or 0600 for secret-ish names, also when sanitising renamed them: `.npmrc` arrives as `npmrc`,
  0600). Older staging folders convert on their next upload.
- Session workspaces (`DATA/sessions/<id>/workspace`, `DATA/worktrees/<id>`) are the agent's
  working copy and keep normal modes, inside 0700 parents.
- Model downloads: new folders under `model_dirs` are 0700 and the `.part` (so the finished GGUF)
  0600; a resumed 0644 `.part` is narrowed. Sharing `~/models` with another user needs a chmod.
- Writers that used a fixed `.tmp` name with `os.WriteFile` (session batch files, the energy
  sessions file, the agent inbox ledger) use `CreateTemp`, chmod 0600, rename
  (`platform.WritePrivate`).
- A config folder that existed before Cerberus (made by hand, 0755) is not narrowed; the files in
  it are 0600 and the doctor checks them.
- With `engines.default_model` unset, the alias `default` means the only loaded model, else the
  only model; with several it stays unknown and the error says to set `engines.default_model`
  (a fresh install's first chat used to fail with "unknown model: default").
- Walkthrough fixes (UI only): the Diff tab aligned with the other tabs; version labels get a "v"
  only for release numbers; a missing transcripts folder on Usage is an info note, not an error;
  the leaderboard's empty Cost view says local requests cost energy and suggests Energy or Tokens;
  the terminal input says "Queued" or "Busy" instead of "Session closed" for a session not running;
  Diff on a scratch or folder session shows "No diff" with Open Files without calling the API; the
  welcome opens with focus on Next; toasts are opaque so they read over page actions; energy under
  0.05 J reads "<0.1 J", not "0.0 J".
- Idle, measured from `/proc` over 60 s after 20 s idle, no browser: 18 to 22 MB RSS, 0.01 to 0.02 s
  CPU (one 30 s monitor sample), about 6 context switches a second; with one idle SSE client only
  the 20 s keep-alive wakes. Every Cerberus goroutine was parked in select or a channel receive.
- A config that has no `init_done` field counts as done, so the first-run welcome needs
  `"server": {"init_done": false}` written explicitly.
- Review fixes: an upload into a held staging folder records a private name in the list before
  the file is linked in, and fails (leaving no file) when that record cannot be written. Linking a
  file into a staging folder is held against a Create handing the folder out
  (`session.Manager.PlaceStaging`): the Create waits for uploads placing a file, and an upload that
  comes while the Create holds the folder is refused (409, "attach the file again").
- Sessions follow the implicit `default` too: a task for a profile with `needs_model` and no model
  picked gets the model `default` resolves to when `engines.default_model` is unset (an escalation
  to such a profile gets `default`, resolved by the gateway); with several models it is refused
  with the engine's "set engines.default_model" error.
- `config.Save` and the token rotation write `config.json` with `platform.WritePrivate` too, so a
  stale 0644 `config.json.tmp` can no longer leave the token readable.
- `cerberus help CMD SUB` equals `cerberus CMD SUB -h`; `cerberus version -h` prints its usage;
  an unknown command exits 2 also when help is asked for (`cerberus help bogus`).
- Energy headless check (`web/test/energy-headless.test.mjs`, run by `web/tools/pwa_headless.sh`):
  each polling view is mounted, the page is made hidden, and no EventSource, interval or timeout
  over a second may remain, nor any request in the next 3 s; showing it again reopens the event
  stream. Chat while a reply streams needs a model and is not covered (its frame clock is
  unit-tested). It found the navbar clock (1 s) and greeting (60 s) intervals running while
  hidden; the local `carino-clock.js` and `carino-navbar.js` copies now stop them while hidden (the
  fleet's canonical copies still need the same change). Monitor's series and Usage's watch
  loops schedule only from their newest call, so a hide/show during a request no longer starts a
  second loop.

## Model problems in the chat (after the day plan)

- **Engine dropped the model:** when a local engine answers "not loaded" (LM Studio's "No models
  loaded", "model … not found", `model_not_found`) before anything was sent to the client, the
  gateway marks the model unloaded (`engine.Manager.Forget`), loads it and retries once. A second
  failure is relayed as it is. Other 400s are never retried.
- **"default" is pinned:** a chat on `default` keeps the model that answered its first message
  (`chat.Options.ResolveModel`), so a later unload cannot leave `default` ambiguous mid-chat.
- **Tools stay on:** a missing-model error is no longer read as "this model takes no tools", so no
  "No tools this time" note and no switch to plain chat.
- **Chat UI:** a failed run caused by the model shows a card with the fixes (Load and retry, Pick a
  model, Retry) and the engine's own words folded under it; only the newest card keeps its buttons.
  Before sending, a hint says when no model would answer (on `default`, no default set, and not
  exactly one model loaded). The picker offers "Make default" when no default is set. With the phone
  keyboard open, a model chip stays on the title row. The keyboard state is re-checked on focus,
  visibility and resize, so the tools come back when the keyboard is gone.
- Not changed: an Ollama that starts after Cerberus can stay "down" for up to 30 s (probe cache), and
  the UI then shows "unknown model" with the same card.

## Delegation to local models and training examples (after the day plan)

- **Tools:** `cerberus_models` (local models agents may use: loaded or not, context, measured
  tok/s from 30 days of usage, owner tags, plus `delegate.guidance`), `cerberus_ask` (one job to a
  local model: prompt, optional system line, up to 8 workspace files read through `os.Root` and the
  deny paths, no tools on the model's side) and `cerberus_rate` (good, or a better answer; only for an example the
  session itself asked for, at most 3 verdicts each, else 404/403/429). They are
  offered with the six session tools to every agent that gets them (Claude Code through
  `cerberus mcp`, the built-in agent), and they call the agent scope with the session's own token.
- **Asks** go through the daemon's own gateway in-process (loaded, queued, measured and recorded like
  any request) as client `cerberus-delegate` with the caller's session. "local" picks the best
  loaded model. A model that is not loaded answers 409 at once unless `delegate.load`; more than
  `delegate.max_queue` asks in flight answer 429. The tool turns both into "do this job yourself".
- **Savings:** `GET /api/delegation` and `/api/sessions/{id}/delegation` sum delegated requests
  (jobs, tokens, energy, time) and price them at `delegate.reference_model`. The session view shows
  a "delegated" line and Usage a "Delegated to local models" section with a per-model table.
- **Training examples** (`finetune.capture`: off by default; "delegated" keeps asks and ratings;
  "cloud" also keeps cloud chat answers through the gateway, skipping tool traces) go to
  DATA/finetune as 0600 JSONL with vault values masked. Export (`cerberus finetune export`,
  `GET /api/finetune/export`) writes chat fine-tuning JSONL: cloud answers, delegated answers rated
  good, and the better answer where one was given; answers rated bad never. Cerberus ships no
  trainer: `cerberus finetune train` exports into a run folder and runs `finetune.train_command`
  with `{dataset}`, `{base_model}` and `{out}`.
- Not built yet: offering the tools to opencode and aider over MCP.

## Navigation, Settings and versions (after the day plan)

- **Order:** Chat is the first view and where the UI opens (an unknown address lands there too),
  then Sessions, Wall, Models, Monitor, Usage and Settings. Phones: Chat, Sessions, Monitor, More.
  `#carino-bridge` still lands on Sessions, where received files pick a session.
- **Settings** gains two groups after Security: **Secrets** (the vault view, embedded; `#/secrets`
  redirects to `#/settings/secrets`) and **About** (version, build, host, uptime, what Cerberus is,
  links). The shared navbar's About button is hidden in the app (CSS only; carino-navbar.js is
  unchanged), and the page no longer carries a `[data-carino-about]` block.
- **Monitor:** the Tasks table opens in a wide popup ("Show tasks…"). Processes are read only while
  the popup is open, never in the background.
- **Versions:** pre-release `0.0.<number of commits>` (no release tags yet). `make build` sets
  `main.version` to that and `main.build` to `git describe --always --dirty` (the short commit, with
  "-dirty" for uncommitted changes). A plain `go build` says `0.0.0 (dev)`. `cerberus version`,
  `GET /api/status` (`version`, `build`) and Settings → About show both.

# Flow view and fine-tuning (after the day plan)

Two owner-facing pieces on top of delegation (see "Delegation to local models and training
examples"): a live, read-only **Flow** view of who handed what to whom, and a **Fine-tuning** page
to review training examples and run the owner's trainer. Read the "As built" sections for
delegation, navigation and model problems first; they describe the code this builds on.

## Flow: the graph (server)

- `GET /api/sessions/{id}/flow` and `GET /api/chats/{id}/flow` (owner scope, read-only) return
  `{nodes, edges, updated}` for that session's whole tree (its root ancestor and every descendant)
  or that chat:
  - **nodes** `{id, kind, label, sub, state, href}` with kind one of `you` (the person, one per
    graph: the task or the chat's first message as sub), `session` (agent sessions: agent, model,
    state, workspace), `chat`, `model` (local or cloud models that answered delegated jobs; sub =
    backend; state = loaded or not), `tool` (optional: MCP tool groups if cheap to derive).
  - **edges** `{from, to, kind, label, count, good, bad, corrected}` with kind `task` (you →
    root), `spawn` (parent → child session), `ask` (session or chat → model, aggregated per model:
    jobs, tokens, and ratings from the fine-tuning store when kept), `answer` (model used by a
    session's own agent or a chat, from usage records by session).
  - Built from the session manager (Parent links, states), usage records (client
    `cerberus-delegate` and the session's own requests, by session id), and the finetune store's
    ratings. No new persistence. A session id the caller cannot see answers 404.
- Live: the UI re-fetches on the existing event stream's session events for ids in the graph and on
  usage of those sessions (debounced to at most one fetch per 2 s), never by polling while hidden.

## Flow: the view (web)

- A **Flow** tab in the session view (beside Terminal, Files, Replay, Diff) and a Flow button in the
  chat header that opens the same graph as a sheet. Route `#/s/<id>/flow`.
- Drawn as SVG by a new self-contained module (`web/dist/assets/js/flowgraph.js` for the pure layout
  and geometry, `views/flow.js` for the DOM), written fresh in Cerberus (no code copied from Topo):
  - n8n-like rounded node cards: an icon (inline SVG, drawn here), label, sublabel, a state pill;
    the root "you" node and model nodes are circles; ports as small dots on the left and right.
  - Cubic edges from right port to left port; `spawn` solid, `ask` dashed, `answer` dotted; a label
    chip in the middle ("2 jobs · 👍1 ✎1", "spawned"); arrowheads.
  - Layered left-to-right layout (longest path layering, then order within a layer by parent order
    to reduce crossings); on phones (< 640 px wide) the layout runs top-to-bottom.
  - Dotted grid background; pan by drag (mouse or one finger), zoom by wheel and two-finger pinch
    (Pointer Events, zoom around the pinch midpoint), "Fit" button, double-tap/double-click to fit.
  - Tap a session node to open that session; a model node opens Models.
  - Carino tokens for colours, Gold and Colourful themes both legible; text never overflows a card
    (ellipsis + title).
- Pure functions (layout, edge geometry, label text, fit transform) are unit-tested with node.

## Fine-tuning: server

- `GET /api/finetune/examples?kind=&rated=good|bad|corrected|unrated&q=&limit=&before=` (owner):
  newest first, each `{id, time, kind, model, session, messages, answer, rating}` with messages and
  answer clipped to 4 KiB each in the list (`truncated: true`); `GET /api/finetune/examples/{id}`
  returns one in full.
- `POST /api/finetune/examples/{id}/rate {good, better, note}` (owner): a person's verdict; it is a
  rating like an agent's (the newest wins) with `by: "owner"`.
- `POST /api/finetune/examples/{id}/exclude {exclude: bool}`: excluded examples never export (kept
  on disk; a rating line with `exclude`).
- `POST /api/finetune/train {base?, unrated?}`: exports the usable rows into a new run folder and
  starts the configured `finetune.train_command` as a **session** so it gets the terminal, replay,
  notifications, Stop and the Wall for free: a scratch session titled "Fine-tune <base model>" with
  an internal agent profile `finetune-train` (not shown in agent pickers) whose command is the
  expanded train command, run in the run folder copy (the dataset is in the workspace). 409 when no
  trainer is set or there are no usable rows; the reply is the session Info. `cerberus finetune
  train` keeps working as before (runs in the terminal).
- `GET /api/finetune/runs`: past runs (time, rows, base model, session id, state, output folder).
- Tests: list filters and paging, owner rating overriding an agent rating, exclusion from export,
  train refusal cases, train starting a session with a fake trainer script (never a real trainer,
  never a GPU), run listing. Route matrix classification and audit for every new route.

## Fine-tuning: the page (web)

- A **Fine-tuning** group in Settings (after Secrets, before About), built like the Secrets and
  About panels:
  - Collection: the capture mode (off / delegated / cloud) as a segmented control saved through
    `PATCH /api/config`, with one plain sentence on what each keeps; counts (examples, rated, good,
    corrected, ready to train).
  - Examples: a filterable list (kind, rating state, search) with each prompt's first line and the
    answer; open one to read it whole; buttons **Good**, **Bad**, **Correct…** (write the better
    answer), **Exclude**. Paged, 20 at a time.
  - Train: the trainer command and base model (read-only summary with a link to the Advanced
    settings), **Download JSONL**, and **Train now** → starts the run and opens its session; past
    runs below with their state and a link.
- Phone layout at 360/390 without horizontal scroll; keyboard reachable; empty states that say what
  to do next.

## Acceptance

- Go: flow graph builder unit tests (tree, asks aggregation, ratings, unknown id), finetune API
  tests, train-as-session test with a fake trainer, route matrix + audit, full gate green.
- Web: node tests for layout/geometry/text and the fine-tuning list helpers.
- Screenshots on the demo daemon (`/tmp/claude-1000/cerberus-site/demo`: fake Ollama on 7398 with
  scripted replies, `bin/demo-agent`, `bin/planner-agent` that drives `cerberus mcp` to delegate and
  rate): the Flow tab for a planner session with a child and two models at 1280 and 390, the chat's
  Flow sheet, the Fine-tuning page with examples, a correction being written, and a train run
  session with a fake trainer, in Gold and Colourful.


## Flow view and fine-tuning: as built

Only behaviour that differs from, or goes beyond, the sections above.

- **Graph JSON:** edges also carry `tokens` (prompt plus completion; 0 when unknown). Node ids are
  prefixed by kind: `you`, `s:<session>`, `c:<chat>`, `m:<model>`. `answer` edges point from the
  session or chat to the model, like `ask`, so the graph has no cycles. Model `state` is `loaded`,
  `unloaded`, `cloud`, or empty when the engine no longer lists the model. No `tool` nodes.
  `label` is a plain default caption; the view builds its own chip from the counts. An example
  with a better answer counts as `corrected` whatever its good flag says; an edge's `count` is the
  larger of usage requests and examples, so ratings outlive aged-out usage. `updated` is RFC 3339
  UTC. Unknown or malformed ids and agent tokens get 404; no session or chat service gets 503; a
  disabled chat gets 403.
- **Chats in the graph:** chat requests now name the chat in `X-Cerberus-Session`, so usage is
  recorded by chat id and the chat's answer edge shows real requests and tokens. Chats with no
  usage rows yet fall back to one edge counting assistant replies ("N replies"). Chat ids are
  attribution only: the leaderboard ranks them as direct traffic and energy does not charge them.
- **Live refresh:** `/api/events` gained a `usage` event (`{session}`), coalesced to about one per
  second per session per stream, sent when a gateway request charged to that session finishes.
  The Flow tab re-fetches on `session` events for ids in the graph or their children, on
  `sessions`, `events-open` and `usage`; at most once per 2 s, never while hidden (a change while
  hidden waits for the page to show again). The chat's sheet re-fetches when the chat's stream sends
  `done`.
- **Layout:** an edge that skips a layer runs through a lane of its own in each layer it passes, so
  it never crosses a card. Chips that would overlap slide along their own curve. On phones the
  session's graph gets at least 62 % of the screen height (the page scrolls to it) and the fit
  is top-aligned.
- **Dialogs** (app-wide) now close when the route changes, so a sheet never stays over another view.
- **Owner `POST /api/sessions`** ignores `Parent`; only an agent's spawn sets it.
- **Fine-tuning API:** the list answers `{examples, total, next}` (pass `next` as `before`; it is ""
  on the last page); each example has `excluded`, and `rating` is null when unrated. Rate and
  exclude answer with the updated example. Agent ratings are stored with `by: "agent"`; older lines
  without `by` read as an agent's. Exclusion is an append-only `{id, exclude}` line in
  ratings.jsonl that is not a verdict (a build older than this one would read it as "bad").
  The stats gain `excluded`, and `usable` leaves excluded examples out.
- **Train:** answers 201 with the session Info and an `X-Cerberus-Run` header; 400 for a base with
  control characters, braces or over 200 characters; 409 also when the trainer binary is missing.
  Runs live in DATA/finetune/runs/<local stamp>/ (`-2` for a second run in the same second) with
  `dataset.jsonl` (0600) and `run.json` (the command with vault values masked); `{out}` is that
  folder, `{dataset}` a copy in the session's scratch workspace. The trainer session runs once,
  with sandbox `none` even under `sandbox.strict` (like the CLI; a sandbox would hide `{out}` and
  the GPU), tagged `finetune`. Deleting the session keeps the run folder.
- **Runs list:** `{runs: [{id, time, rows, base, unrated?, session?, dir, dataset, out, command?,
  state}]}`; `state` is the session's, `removed` once it is deleted, or `cli` for runs started by
  `cerberus finetune train`.
- **Internal agent:** `finetune-train` is refused for create, spawn, batch and `escalate_to` (400)
  and never listed, even if the config defines a profile with that name.
- **Page:** the Usage page's delegation block links to Settings › Fine-tuning. Train now asks first
  (confirm dialog), then opens the run's session.

# Settings import and export (as built)

- Settings → **Import & export** (`#/settings/transfer`) is the web side of `cerberus export` /
  `cerberus import`. Export picks sections (all by default) and optionally secret names; values of
  secrets never leave. Routes (owner): `GET /api/config/export`, `POST /api/config/import
  {bundle, apply}`, `POST /api/config/models/forget {ids}`; contract in `internal/api/bundle.go`.
- The bundle has a new `models` section: every local model the engines list, with `ref`, the
  reference that downloads it again (Ollama/LM Studio name; for llama.cpp the URL in the
  `<file>.gguf.source` note a download now leaves, else rebuilt from `<owner>/<repo>/<file>` under a
  models folder), and its `models[]` entry as `config` when there is one. The CLI export has no
  engine inventory and writes only the `models[]` entries.
- Import adds the `models[]` entries (a llama.cpp path that does not exist here is pointed at where
  its download lands) and never downloads or deletes anything. The page then lists each model as on
  this machine or missing; a missing one can be **Downloaded** (the normal download queue) or
  **Removed** (its `models[]` entry leaves the registry; no file is touched). `cerberus import`
  prints a `cerberus pull` line for each model instead.

# Sessions and Wall merged (as built)

- The Wall tab is gone: Sessions has an **Active / All** filter with counts. Active shows the active
  sessions' live tiles as a grid of small tiles and only those sessions in the list (with "Nothing is
  running now · Show all" when none are); All lists every session with the tiles as one short strip.
  A visit starts on Active when something runs, else All (a layout switch was tried first and
  dropped: with nothing running it changed nothing). A tapped tile fills the app with its input bar
  (← Sessions goes back). The tiles are `views/wall.js` mounted with `embedded`.
- `#/wall` opens Sessions on Active. `#/wall/<ids>` (the bulk bar's Wall button, Choose…) stays the
  full-screen watch page for a chosen set. The short-lived inactive cards on the Wall were dropped:
  the list below the tiles replaces them.

# Flows: designed, scheduled and triggered tasks (phases 1 and 2)

A **flow** is a saved recipe: a trigger plus a list of steps that hand work to models and agent
sessions. It builds on delegation (`callLocal` through the in-process gateway), agent sessions,
the inbox and the Flow view. Phase 1 is the engine, its API, the CLI and the MCP fix; phase 2 is
the **Flows** view (designer, runs, templates). Chat-intent triggers and per-chat/per-model
prompt enrichment are phase 3; event triggers and a `cerberus_run_flow` MCP tool are phase 4.
They are out of scope here, but the formats must not block them.

Words: *flow* = the recipe; *run* = one execution; *step* = one node. The read-only session graph
stays "Flow view"; a run's graph is drawn by the same renderer (`flowgraph.js`).

## Format (internal/flows)

- A flow is `DATA/flows/<id>.json` (0600), id `fl-<slug>`, written atomically, with
  `{id, name, description, enabled, trigger, input_hint, limits, steps[], version, updated}`.
  Every save bumps `version`; the previous file is kept as `<id>.<version>.json` (last 10).
- **trigger** `{kind, schedule?}`: `manual` (button, CLI, API) or `schedule`. A schedule is a
  string: `every 6h` / `every 30m`, `daily 08:00`, `weekdays 08:00`, `mon,thu 19:30`, or a 5-field
  cron line (minute hour day-of-month month day-of-week, numbers, `*`, lists, ranges and `/n`
  steps). Local time. A scheduled flow runs with its `input_hint` (or empty) as input. Missed runs
  while the daemon was down are not caught up (one run at most, noted in the run list).
- **steps**: an ordered list; each `{id, kind, name, ...}`; ids are short slugs, unique in the flow.
  Steps run in order; `if` and `fan_out` are the only control flow. Kinds:
  - `prompt` `{model, system?, template, json?}`: one chat completion through the gateway (the
    model may be local or a cloud provider; `default` allowed). Output = the reply text (or the
    parsed JSON when `json` is true).
  - `plan` `{model, template, max_items}`: like prompt, but the reply must be a JSON array of
    `{title, kind, task}` (the step asks for it and extracts the first JSON array; one retry with
    the parse error). Output = the items.
  - `fan_out` `{from, routes[], default_model, as: "prompt"|"session", template, agent?, workspace?,
    max_parallel}`: for each item of step `from`'s output, pick a model by the first route whose
    `kind` matches (`{kind, model}`), else `default_model`, and run a prompt (or a session) with
    `template` (`{item.title}`, `{item.task}`, `{item.kind}`). Output = the list of results.
  - `session` `{agent, workspace: scratch|repo|folder, repo?, folder?, task}`: starts an agent
    session (profile, model as the profile says) with the rendered task as a child of nothing, tagged
    `flow:<flow id>`, and waits for a final state (passed/failed/stopped/...). Output =
    `{session, state, summary}` (summary = the last activity line). Never auto-accepts.
  - `if` `{test, then: step id, else: step id}`; test is one of `contains` / `matches` (regex, RE2)
    / `equals` / `json` (a dotted path equal to a value) / `state` (a session step's final state),
    against `{steps.<id>.output}`. Jumping only forward; a jump to "end" stops.
  - `approve` `{message}`: puts an inbox item with Approve / Reject and a notification, and waits.
    Reject ends the run as `rejected`.
  - `output` `{to: "notify"|"inbox"|"file", template, file?}`: a notification, an inbox entry, or a
    file in the run folder (name sanitised, inside the run folder only).
- **Templates** are plain text with `{input}`, `{steps.<id>.output}` (JSON-encoded when not a
  string), `{item.*}` inside fan-out, `{date}`, `{time}`, `{flow.name}`. No code runs; unknown
  placeholders are an error at save time (validation) and render as empty never.
- **limits** `{max_steps 50, max_tokens 200000, max_cost_usd 1.00, max_parallel 2, timeout_min 60}`
  with these defaults; each step's model calls go through the gateway (usage recorded with client
  `cerberus-flow` and the run id as session), and a run that passes a limit stops as `limited` with
  the reason. Cost uses the existing prices; local models cost energy only.
- **Validation** (save and run): known kinds, unique ids, references to earlier steps only, models
  and agent profiles that exist (warning when the model is not installed now), schedule parses,
  template placeholders known, fan-out `from` points at a plan (or json prompt) step.

## Runs

- A run is `DATA/flows/runs/<run id>/run.json` + one `steps/<n>-<step id>.json` per finished step
  (input as rendered, output, model, tokens, cost, energy, timings, error), plus the output files.
  States: `queued`, `running`, `waiting` (approve), `passed`, `failed`, `rejected`, `limited`,
  `canceled`. A daemon restart marks running runs `failed` ("daemon restarted") unless they were
  waiting for an approval, which keeps waiting.
- At most `flows.max_runs` (default 2) runs at once; more queue. A scheduled run is skipped (and
  says so) when the guard refuses new sessions or the previous run of the same flow is still going.
- Every start, approval, cancel and flow change is in the audit log.
- Live: run and step changes go out on the event stream as `flowrun` events `{run, flow, state,
  step}`.

## API (owner; route matrix and audit)

- `GET /api/flows` (with next scheduled time and last run), `POST /api/flows` (create),
  `GET|PUT|DELETE /api/flows/{id}` (PUT with `version` for optimistic concurrency: 409 when stale),
  `POST /api/flows/validate` (problems for a draft, nothing saved), `GET /api/flows/templates`.
- `POST /api/flows/{id}/run {input}` → the run; `POST /api/flows/test {flow, input}` runs an
  unsaved draft once (marked test, never scheduled).
- `GET /api/flows/runs?flow=&limit=`, `GET /api/flows/runs/{run}` (run + steps),
  `POST /api/flows/runs/{run}/cancel`, `POST /api/flows/runs/{run}/approve {ok, note}`,
  `GET /api/flows/runs/{run}/flow` → `{nodes, edges, updated}` in the Flow view's shape (one node per
  step with state; fan-out items as child nodes; model nodes for models used).
- Config (add-only, with schema metadata, advanced): `flows.enabled` (true), `flows.max_runs` (2).

## CLI

`cerberus flow list | show ID | run ID [--input TEXT] [--wait] | runs [ID] | cancel RUN |
approve RUN [--reject]`, through the daemon. Help text for each.

## MCP fix

`cerberus mcp` speaks 2025-06-18 and expects `initialize`. It must also serve clients on the
2026-07-28 spec: answer `tools/list` and `tools/call` without a prior `initialize`, answer
`server/discover` (server info, capabilities, supported protocol versions), and echo the client's
requested protocol version when it is one we support (2025-06-18, 2026-07-28). Tests for both.

## The Flows view (phase 2, web)

- A **Flows** tab after Sessions (phone: in More). Routes `#/flows`, `#/flows/<id>`,
  `#/flows/new`, `#/flows/runs/<run>`.
- List: each flow's name, trigger in words ("Weekdays at 08:00", "Manual"), next run, last run state,
  enabled switch, **Run** (asks for input when the flow has an input hint). Recent runs below.
- Editor: name, description, trigger (Manual / Schedule with a friendly picker and a cron field),
  input hint, limits (collapsed), and the steps as an ordered list of cards (add, duplicate, move
  up/down, delete); each card is a short form for its kind with model and agent pickers. A live
  graph preview beside it (below on phones) drawn with `flowgraph.js`; validation problems inline.
  **Test run** with sample input runs the draft and shows each step's output as it arrives. Save
  with version check ("changed elsewhere" on 409).
- Templates to start from: **Clarify → Answer**, **Draft → Critique → Revise**,
  **Plan → Delegate → Merge**, **Website** (plan with a big model, fan out HTML/CSS and copy to local
  models, assemble in a session, approve, output).
- Run page: the run graph (live), each step's input and output (collapsible, copy), tokens, cost,
  energy, Approve / Reject when waiting, Cancel, links to sessions it started.
- 360/390/1280 without horizontal scroll, Gold and Colourful, keyboard reachable.

## Acceptance

- Go: schedule parser (friendly forms and cron, next-time computation incl. DST), templates,
  validation, each step kind with fake models and a fake agent (never a GPU), limits, fan-out
  parallelism, approve/reject, cancel, restart recovery, run graph, API matrix and audit, CLI, MCP
  both protocol versions. Full gate green.
- Web: node tests for the pure helpers (trigger text, step forms model, validation display, graph
  conversion).
- Screenshots on the demo daemon (fake engine on 7398): the Flows list, the editor with the Website
  template, a test run in progress and finished, an approval waiting, at 1280 and 390, Gold and
  Colourful.

## As built (Flows, phases 1 and 2)

Only what differs from, or settles, the text above.

- **Format.** A step is one flat record; each kind reads its own fields. `if` is stored as
  `{test, on, value, path?, then, else}` (`on` = an earlier step id); an empty then/else means the
  next step, and a branch runs on in order from its target to the end. `contains` ignores case,
  `equals` trims spaces. A `fan_out` with `as: "session"` also takes `repo`, `folder` and `agent`.
  A placeholder is `{name}` (a letter, then letters, digits, `.`, `-`, `_`); any other brace is plain
  text, so JSON in prompts works. A step skipped by an `if` renders as "".
- **Schedules.** `every Nm` / `every Nh` count from local midnight like cron `*/n` (at most 24h).
  Also `weekends HH:MM` and day ranges (`fri-sun`). A time inside a spring-forward gap runs once just
  after the jump; a repeated fall-back time runs once. After downtime, at most one catch-up run
  starts, with a note saying how many were missed. A skipped scheduled run (guard refused, previous
  run still going) creates no run: it is audited as `flow.run.skip` and shown as the flow's last skip.
- **Runs.** On restart, queued runs fail ("daemon restarted") like running ones; waiting runs keep
  waiting, hold no slot or goroutine, and resume after the approve step. `max_steps` counts steps
  plus fan-out items; `timeout_min` counts running time, not time waiting for an approval. The first
  failing fan-out item cancels the rest. A session step passes whatever the session ends as (branch
  on it with an `if` state test, whose value must be a session final state). Output files go to
  `<run>/files/` with `-2`, `-3` on name clashes.
  Finished runs are kept per flow (newest 100, by creation); the run that has just ended is always
  kept, even when it waited longer than newer runs took to finish. Deleting a flow keeps its runs.
  Test runs of unsaved drafts carry the flow id `fl-draft`, which no saved flow can get.
- **Session usage counts toward the limits.** The daemon's session runner is also a meter: every
  usage record the gateway adds under a session the run started (session steps and fan-out items
  run as sessions) is charged to the run as it comes, and shows on the step or item. A session that
  takes the run past `max_tokens` or `max_cost_usd` is stopped and the run ends `limited`. Usage an
  agent spends outside the daemon (an agent CLI that calls its vendor directly) is not seen, so it
  is not counted.
- **Sessions.** Session tags must match `[a-z0-9-]{1,24}`, so a flow's sessions carry the tags
  `flow` and the flow id (cut to 24 characters) instead of `flow:<id>`.
- **Approvals.** The inbox item (kind `flow-approve`) has Approve / Reject in the bell list while it
  is unread; the daemon marks it read once the run is decided or canceled. The reminder goes to
  desktop/push only, so it is not a second inbox item.
- **API.** `GET|POST|PUT /api/flows/{id}` answer `{flow, problems, trigger_text, versions}`;
  `POST /api/flows/validate` answers `{problems, graph}`; run lists leave out the flow snapshot; a
  stale PUT is 409 with the stored flow; starting a run while `flows.enabled` is off is 403; run
  starts answer 202. The enabled switch is a PUT of the whole flow (no PATCH route).
  `GET /api/status` gains `flows_enabled`. Engine events (`flow.create` … `flow.run.skip`) are in the
  audit log next to the per-route records. Cost and energy per model call come from the gateway's
  usage record (client `cerberus-flow`, session = run id).
- **Run graph.** Node kinds `input`, `step`, `item`, `model`, `end`; edge kinds `next`, `then`,
  `else` (dashed), `item`, `ask`. The run page draws it with a flow legend (next/then, else/model).
- **CLI.** Extra: `flow approve --note`, `flow runs --limit`, aliases `flows` and `flow ls`. `run
  --wait` prints the last step's text, or the file a file output wrote.
- **Templates.** All four use model `default` and agent `cerberus`. In Website, a failed Assemble
  session ends the run early as passed (the run graph shows the session's state).
- **MCP.** An `initialize` naming an unsupported version gets 2025-06-18 (the newest version with a
  handshake); other requests naming one get -32022 with `{supported, requested}`. A modern
  request's `_meta` client capabilities are not required.

# As built: MCP clients (outside agents on the agent scope)

`cerberus mcp` was session-only: every tool goes through `/api/agent/*`, which takes only a
session agent token. An **MCP client** lets an outside agent (Claude Code, say) use the same
nine tools as itself, without ever holding a person's credentials.

- **Token.** A device paired with scope `mcp` (`devices.ScopeMCP`, field `scope` in
  devices.json). Its token starts `ccl-` (full devices: `cdv-`), and `Verify` requires the
  prefix to match the stored scope. An older daemon that ignores `scope` never takes a
  client token for a device, and a scope this build does not know opens nothing.
  `identify` refuses a client token, so the control API, the UI, cookies and `/v1` stay
  closed to it. `agentAuth` hands `ccl-` tokens to `clientAuth`, which refuses an
  `X-Cerberus-Session` header and admits the caller as `kindClient`. The audit actor is
  `client:<name>`; revocation (`devices rm`) cuts its live requests.
- **Scope.** The six session endpoints take a caller that is a session or a client
  (`agentCaller`). A client's sessions are roots with `Info.Client = "mcp:<name>"`, in the
  scratch workspace only (`same` → 400). The client sees and steers those and their
  descendants (`ClientSessions`, `IsClientSession`), nothing else (404).
  - The spawn rules apply: `agent.spawn.enabled`, `require_approval`, a task or model
    starting with `-`, and at most `max_children` open sessions per client. Each
    client session's own tree has the usual depth and total limits.
  - Files and runs (`agentReady`) answer 403.
  - Delegation works, and usage and examples are charged to session `mcp:<name>`. The
    daemon refuses `files` for a client.
- **CLI.**
  - `cerberus pair --mcp [--name claude-code]` pairs through the daemon, or through the
    store when the daemon is down. It saves the token to
    `<config dir>/mcp-clients/<name>.token` (0600, inside the protected config folder) and
    prints the `claude mcp add` line; the token itself is never printed.
  - `cerberus mcp --client NAME [-config PATH]` takes the URL from `server.listen` and the
    token from that file. An inherited `$OPENAI_API_KEY` or `$CERBERUS_URL` is not used.
    `cerberus_ask` reads `files` on the client's side, relative to the MCP server's folder:
    text only, `delegate.max_file_kib` each, at most 8, never a path in `agent.deny_paths`,
    the config folder or the usual secret places (`.env`, keys, `.ssh`, `.aws`, …), checked
    on the path and on its symlink target. The text never enters the calling agent's
    context.
- **UI.** The Security page marks a client with an "MCP client" pill; `cerberus devices`
  appends "(MCP client)".
- **Tests.**
  - `internal/api/mcpclient_test.go`: the scope, pairing rules and delegation.
  - `cmd/cerberus/mcpclient_test.go`: pair, then a stdio round trip from the config alone.
  - `internal/agent/localfiles_test.go`: client-side file reading.
  - `devices` scoped tokens; `web/test/security.test.mjs` for the pill flag.

# As built: copying logs (flows and sessions)

- **Flow runs.** The run page and the editor's Test run panel have **⧉ Copy log** and
  **↓ Log**: the whole run as plain text (`runLog` in `flows.js`). It holds a header
  (flow, run, state, trigger, times, usage, models, reason, approval, input), then every
  step in flow order with its state, usage, error, jump, session, file, input and output,
  and fan-out items indented inside their step. Nothing is cut; the per-block view still
  shows 20,000 characters. The download is named `<flow>-<run>.log`.
- **Sessions.** The session page has **⧉ Copy log** and **↓ Log** over the existing
  `GET /api/sessions/{id}/transcript`: plain text, terminal codes stripped, secrets
  masked. The copy fetches at the click, and the clipboard write starts inside the click
  through `ClipboardItem` with a promise, as Safari requires. The download is
  `cerberus-<id>.log`.
- **Copy over plain http.** The Clipboard API exists only on secure pages (HTTPS or
  localhost). `copyText` now falls back to a hidden textarea and `execCommand('copy')`,
  so every Copy button works on a tailnet `http://` address too. When both fail, it says
  to use Download.
- Tests: `web/test/flows.test.mjs` (`runLog`, `runLogName`). Checked on the demo daemon:
  the clipboard equals the download, the session copy equals the transcript, and the
  fallback works with `writeText` refused. No console errors, and no horizontal scroll at
  390.

# As built: fan-out retry and waiting (flows)

The 2026-11-03 failure: a Website run fanned three prompts out to a freshly loaded local
model, the engine ran out of GPU memory, all three calls came back "terminated", and the
fan-out cancelled its siblings on the first error. The flows half of the fix:

- **One retry on a transient engine failure.** A prompt or plan step, or a fan-out item
  run as a prompt, whose model call fails with a temporary engine error is retried once,
  after 2 s on the engine's clock. "Transient" is the engine's rule (`engine.IsTransient`,
  and `engine.IsTransientText` on the gateway's status and message): an engine crash,
  "Channel Error", "terminated", a dropped connection, a 502 or 503 from the local engine,
  and the room timeout. Limits, validation errors (other 4xx), cancels and session steps
  or items are never retried. Step and item records gain `retries` and `retry_error`.
  `flows.ModelError` carries the status: the flow host's model caller returns it, with the
  same text as before ("model call failed (400): terminated").
- **No sibling cancel on a transient failure.** A transient failure that persists after
  the retry fails only its item; the siblings keep running. Any other failure (and a
  limit) still cancels the rest at once; items stopped that way are now `canceled`, not
  `failed`. The step fails if any item failed, with "N of M items failed: <first error>"
  (a limit keeps its own reason).
- **Waiting for the model.** Each flow model call carries `engine.WithWaitHook` on its
  context, which the in-process gateway hands to `Engine.Acquire`. While the engine holds
  the call, the item's `waiting` and the run's `waiting` (the current step's: the reason,
  or for a fan-out step "item 2: <reason>" / "3 items waiting: <reason>") say why. The
  change is published as a `flowrun` event, at most one per 300 ms per run (a later change
  inside the gap is sent at its end). `GET /api/flows/runs/{run}` adds `live`: the running
  fan-out step's items so far.
- **Graph.** A running step or item whose call is held gets the state `queued` and the
  reason in a new node field `detail` (also "retried once after: ..." on a retried one).
  In the UI `queued`, like `waiting`, takes the `.fl-wait` look (warn stroke, no pulse;
  `isWaiting` in `flowgraph.js`), the tooltip shows the detail, and the run legend has
  "waiting".
- **Run page and log.** A step or item line shows "waiting for the model: <reason>" while
  held, and the usage line ends in "retried once". The running fan-out step lists its live
  items. `runLog` adds "Waited: <reason>" (at copy time) and "Retried: n (after: <error>)".
- **Template.** The Website template's `max_parallel` is 2 (was 3).
- Tests: `internal/flows/retry_test.go` (retry then pass; transient twice keeps siblings
  and fails "1 of 3 items failed"; non-transient fails fast; prompt and plan retry;
  session items not retried; wait hook sets, clears and publishes; throttle; graph
  `queued`), `internal/api/flowhost_retry_test.go` (the context reaches the gateway, the
  error is a `ModelError`), `web/test/flows.test.mjs` and `flowgraph.test.mjs`.


# As built: model queue and room

Engine half of the 2026-11-03 fix. A fan-out sent 3 prompts at once to a 27B model that
LM Studio had loaded with full GPU offload and 4 slots. CUDA ran out of memory, the engine
aborted, and the flow failed.

- **Per-model queue** (`engine/queue.go`). A local model admits at most N requests at
  once. Extra callers wait in `Acquire` in FIFO order, and a cancelled context takes them
  out of the queue. A freed slot goes straight to the oldest waiter.
  - N is `models[].max_parallel` when set.
  - Otherwise N is 1 for a model larger than half the total VRAM, else the engine's slot
    count: llama.cpp `-np`, LM Studio `models[].parallel` or its default of 4, Ollama
    `models[].parallel` or 1.
  - When VRAM is unknown, N is the slot count. An engine that reports no slots (and
    every cloud model) has no limit.
  - `ModelInfo.MaxParallel` and `ModelInfo.Waiting` show the limit and the queue. Their
    JSON names are PascalCase, like the struct's other fields.
- **Wait for room** (`makeRoom`). A load may still not fit after idle models are evicted
  while another loaded local model holds slots. The load then waits for a release (a
  broadcast channel, no polling), evicts the model that went idle, and checks again.
  - A busy model counts as busy until its queue drains. Swapping models between queued
    requests would reload both each time.
  - The wait ends at the caller's context or `engines.wait_for_room_sec` (default 600).
    The timeout returns `ErrRoomTimeout`, which wraps `ErrNoRoom` and is transient.
  - The old spill-to-RAM rule (VRAM + RAM) applies only when nothing busy holds memory,
    when waiting is off (`0`), or when `allow_overcommit` is on.
- **Lock order.** `loadMu` (target) → `roomMu` → `mu`.
  - The queue lives under `mu` alone. A queued request holds no lock, and a release takes
    only `mu`, so a load waiting for room never blocks a busy model from finishing.
  - Requests to loaded models never take `roomMu`. A loaded model whose llama-server
    died is marked unloaded before it queues for `roomMu`, and only loaded models are
    waited on, so a waiting load can never wait on a request that waits for it.
  - `tryUnload` still never waits.
- **GPU split on load** (`engine/gpusplit.go`). The estimate is need = size×1.15 + 512
  MiB + KV, where KV = size × tokens / 256k. LM Studio counts ctx × parallel tokens;
  llama.cpp counts ctx once, because its `-c` is shared by all slots.
  - `lms load` passes `--parallel <N>`. It passes `--gpu <ratio>` only when need exceeds
    the free VRAM left after making room: the ratio is free/need, clamped to 0.10–0.95
    and printed with 2 decimals. When the model fits, it passes nothing and LM Studio
    decides.
  - `engines.lmstudio.gpu` ("" | max | off | 0–1) overrides the ratio.
  - llama.cpp starts with `-np <N>`. When the model does not fit, it gets `-ngl` = layers ×
    ratio, with the layer count read from the GGUF header (`<arch>.block_count`). The
    user's own `-ngl` wins.
- **Crash recovery.** The gateway treats a local engine reply as a crash in these cases:
  a refused, reset or EOF connection; a 502 or 503 (not llama-server's "Loading model");
  or crash words in any status (LM Studio's 400 "terminated", "Channel Error", CUDA out
  of memory).
  - The gateway calls `Manager.MarkCrashed(id)` and answers 503 `engine_failed` "local
    engine failed: <upstream> (it will be reloaded)".
  - The gateway does not retry; the flows runner retries once. A stream that had already
    answered 200 keeps its status, but the model is still marked when the stream breaks
    or sends a crash error event.
  - `MarkCrashed` marks the model unloaded and lowers its limit by one (min 1) for the
    daemon's life. Several requests failing from one crash lower it once. The next load
    unloads what is left first, so LM Studio reloads with the new `--parallel`.
- **Public API.** The wait hook goes through the request context, so in-process gateway
  calls from flows reach it.
  - `engine.WithWaitHook(ctx, f)`; the hook hears "waiting for <id>: n of N slots busy",
    "waiting for room: <models> busy", and "".
  - `engine.IsTransient(err)` and `engine.IsTransientText(status, msg)`.
  - `engine.ErrRoomTimeout` and `(*Manager).MarkCrashed(id) bool`.
- **New settings** (add-only, with schema metadata): `engines.wait_for_room_sec`,
  `engines.lmstudio.gpu`, `models[].max_parallel`.
- **Tests.**
  - `engine/queue_test.go`: FIFO, the limit, cancel, the hook, the default-N table, room
    wait, timeout and cancel, the queue and room under `-race` with no deadlock,
    MarkCrashed, the `lms` args, `-ngl` and GGUF, and the transient tables.
  - `gateway/crash_test.go`: 400 "terminated", Channel Error and 502 map to 503 with one
    mark; cloud is not marked; validation errors are relayed; a refused connection
    counts; a stream error event counts.
  - `config/queue_settings_test.go`.
- **Review fixes.**
  - Crash reports carry the load: `Endpoint.Gen` is the model's load number, and the
    gateway calls `MarkCrashedGen(id, gen)`. A request of the crashed engine that fails
    only after the model was reloaded no longer unloads the fresh engine or cuts the
    limit again. `MarkCrashed(id)` stays and acts unconditionally.
  - The per-model load lock and the room lock give up when the caller's context ends,
    and the wait hook hears "waiting for <id> to load" or "waiting for room: another
    model is loading" while queued on them. Before this, a request queued behind a load
    that waited for room looked live and could not be cancelled for up to
    `wait_for_room_sec`.
  - `makeRoom` notices a busy model that goes idle while it evicts others, and evicts it
    instead of waiting or spilling into RAM.
  - A failed write to the client (it hung up) is not an engine crash.
  - `IsTransientText` treats a 503 "context canceled"/"deadline exceeded" (the caller's
    own context) as not transient. `engines.lmstudio.gpu` refuses NaN.
  - Tests: `engine/queue_review_test.go`, `gateway/crash_review_test.go`,
    `config/queue_review_test.go`.
- **End to end.** `api/flow_engine_e2e_test.go` drives a flow (plan prompt, then a
  fan-out of three prompt items, `max_parallel` 3) through the real flow host, gateway and
  engine Manager, with a fake Ollama that serves one request at a time (a second
  concurrent request gets 400 "Channel Error"). The engine's default limit of 1 runs the
  items one at a time and the run shows "waiting for qwen ..." meanwhile. With one
  400 "terminated" crash, the gateway marks the model, the flow retries that item once,
  the engine unloads and reloads the model, and the run passes.

### As built: answering chat permission requests outside the chat (2026-11-03)

- Before, an LLM chat's permission card ("The model wants to use Bash") could only be answered inside that chat. Now every open card of every chat also shows in the bell, on any page, under "Waiting for you · N".
- `GET /api/chats/asks` (owner, chat on) → `[{chat, title, model, req}]`. Answers still go through `POST /api/chats/{id}/answer`, so the rules, the audit record and "Allow for this session" are unchanged.
- `/api/events` gains an `asks` event, `{keys: ["chat/req", ...]}`. It is sent only when the set of open requests changes, and only to owners while the chat is on. The chat manager pokes it on `ask` and `answered`, and when a wait registers or drops a card (`chat.Manager.SubscribeAsks`, `OpenAsks`, `AskKeys`). The store refetches the list on that event and on every reconnect.
- The bell reuses the chat's own card (`views/chatperm.js`): the 600 ms arming, two presses to run a command, Allow once, Allow for this session, and Deny with a note. Only the Alt+A/S/D hints are hidden, because those keys belong to the chat view.
  - The badge counts open requests plus unread items, and turns amber while a request waits.
  - An answered card stays as its one-line result for 4 s.
  - A request answered elsewhere drops out; answering it anyway gets a 409 and a toast.
- Not added: answering from MCP or agent tokens. An agent must not be able to approve its own requests, so the route is owner-only like the rest of `/api/chats`.

### As built: answering agent session approvals outside the session (2026-11-03)

- Before, the built-in agent's approval prompt (`[y] yes  [a] always <tool>  [n] no`) could only be answered in the session's own terminal. Now it can also be answered from the Sessions list and from the bell, on any page.
- **Agent:** before `waiting;approval` it now sends an `ask` marker (`ESC ] 777 ; cerberus ; ask ; <tool>: <summary> BEL`, at most 300 characters).
- **Session `Info`:** gains `WaitingAsk` (what the prompt is for) and `AskSeq`, which counts approval waits.
  - The ask text is kept only for a wait whose reason is "approval".
  - It is cleared whenever the state leaves waiting.
- **`POST /api/sessions/{id}/answer` `{answer, seq}`:** answer is y, a, n, or one line of text, which the agent reads as a refusal with that reason. `Manager.AnswerApproval` types the answer and Enter only while the session still waits at approval number `seq`.
  - It flips the state to running under the same lock, so a second answer gets 409 (`ErrNoApproval`).
  - A stale page also gets 409.
  - A bad answer gets 400 (`ErrBadAnswer`).
  - The answer is audited as "approval yes|always|no: <ask>" and goes in the input log.
- **UI:** `views/approval.js` provides Yes, Always `<tool>` and No. The buttons arm after 600 ms.
  - On the Sessions list they take the place of the Waiting badge, and the reason line reads "Approve? <ask>".
  - The bell lists every session waiting at an approval prompt under "Waiting for you" (from the store's live session list, so no new stream), together with the LLM chat cards. The badge counts both.
  - `rowKey` now includes `WaitingAsk` and `AskSeq`, so a new prompt redraws the row.
- **Other agents** (Claude Code and the like) do not send these markers. Their waits keep the plain Waiting badge and are answered in the terminal.


---

## Users and roles (Phase 4a, 2026-11-03)

Cerberus gets several user accounts with five roles. A person signs in with a user name and
password. The admin token stays as the break-glass and bootstrap path. Regular users see only
their own work; admins see everything. This section is the contract for the build. It is split
into a BACKEND part (`internal/`, `cmd/`, `docs/USERS.md`, `docs/SECURITY.md`, as-built notes
here) and a WEB part (`web/dist/**`, `web/test/**`, `web/tools/**`). The HTTP contract between
them is in "API contract" below. The public landing site (`index.html`, `site/`) is not part
of this work.

### Goals and limits

- Several accounts on one daemon, one OS user. Cerberus is still one process run as one OS user.
  Confinement of other people's agents is by bwrap and by path checks, not by OS accounts.
- No users yet means nothing changes: the daemon behaves exactly as today (single owner, admin
  token, paired devices). Multi-user mode starts when the first user exists.
- The server is the authority. The UI hides what a role cannot use, but every route checks the
  role and the owner.
- No new Go dependencies. Passwords use stdlib `crypto/pbkdf2` (Go 1.24+).

### Roles

Role names (the JSON and CLI spelling): `admin`, `user`, `viewer`, `monitor`, `chat`.
The UI calls `chat` "Chat only".

| Capability (cap id) | admin | user | viewer | monitor | chat |
|---|---|---|---|---|---|
| Own LLM chats (`chat`) | yes | yes | no | no | yes |
| Chat tools that read/write files or run commands (`chat.tools`) | yes (host) | yes (confined) | no | no | no (plain chat + images) |
| Start and steer own sessions, fleet tasks, uploads (`sessions`) | yes | yes (confined) | no | no | no |
| Read all users' sessions: list, wall, terminal (read-only), transcripts, diffs, files, replays (`sessions.watch_all`) | yes | own only | yes | no | no |
| Type into terminals, approve, start, stop, delete (`sessions.write`) | any | own | no | no | no |
| Flows and flow runs (`flows`) | all | own | no | no | no |
| Monitor page: power, energy, bill, process list (`monitor`) | yes | no | yes | yes | no |
| Signal or renice host processes (`monitor.signal`) | yes | no | no | no | no |
| Doctor full report (`doctor`) / counts only (`doctor.counts`) | full | no | counts | counts | no |
| Models list (`models.read`) | yes | yes | no | no | via chat models only |
| Load, unload, delete, download models (`models.manage`) | yes | no | no | no | no |
| Usage and spend (`usage`) | all users | own | no | no | own (in Account) |
| /v1 gateway with a personal API key (`gateway`) | yes | yes | no | no | no |
| Voice dictation (`voice`) | yes | yes | no | no | yes |
| Own paired devices (`devices`) / all devices (`devices.all`) | all | own | own | own | own |
| Settings, config import/export, restart (`settings`) | yes | no | no | no | no |
| Secrets vault (`secrets`) | yes | no | no | no | no |
| Users admin panel (`users`) | yes | no | no | no | no |
| Audit log (`audit`) | yes | no | no | no | no |
| Fine-tuning (`finetune`) | yes | no | no | no | no |
| Token rotation, prune, repo finder, host folder browser (`host`) | yes | no | no | no | no |
| Import Atrium / Claude Code chats (`chat.import`) | yes | no | no | no | no |

Notes:

- `user` does not get the Monitor page. It shows host-wide processes (command lines) and the
  household bill. The admin can give a person `viewer` for that.
- Admins see every chat in a list filter ("All users"), can open and delete any chat, and can
  stop a running reply. Only the chat's owner can send messages or answer its permission cards
  (the chat acts with the owner's tools and budget). This keeps "chats are private" for every
  role but admin, and keeps an admin from acting as someone else.
- The admin token acts as role `admin` (see Migration).

Each role also has a home view for the UI: admin and user `#/chat`, viewer `#/sessions`,
monitor `#/monitor`, chat `#/chat`.

### Data model (`internal/users`, new package)

Files, all in the data folder (`DATA`, `platform.DataDir()`), mode 0600, written with
`platform.WritePrivate` (atomic temp file + rename). The store re-reads a file that another
process changed (size or mtime), like `devices.Store`, so the CLI can edit while the daemon runs.

`DATA/users.json`:

```go
// File is users.json.
type File struct {
	Version int               `json:"version"` // 1
	Users   []User            `json:"users"`
	Moved   map[string]string `json:"moved,omitempty"` // owner id -> new owner id ("" = data from before users)
}

type User struct {
	ID          string    `json:"id"`      // "u-" + 12 lowercase hex, random, never reused
	Name        string    `json:"name"`    // login name: lowercase, [a-z0-9._-], 1..32, starts with a letter or digit; unique
	Display     string    `json:"display,omitempty"` // shown name, 0..60 runes, devices.CleanName rules
	Role        string    `json:"role"`    // admin | user | viewer | monitor | chat
	Disabled    bool      `json:"disabled,omitempty"`
	Password    string    `json:"password"` // "pbkdf2-sha256$<iter>$<salt b64raw>$<key b64raw>"
	MustChange  bool      `json:"must_change,omitempty"` // set by a generated or admin-reset password
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	PasswordSet time.Time `json:"password_set"`
	LastLogin   time.Time `json:"last_login,omitzero"`
	LastAddr    string    `json:"last_addr,omitempty"`
	Budget      Budget    `json:"budget"`
	Repos       []RepoShare `json:"repos,omitempty"`  // repos the admin shares with this user
	Agents      []string  `json:"agents,omitempty"` // agent profiles allowed; empty = the built-in agent only
	APIKeyHash  string    `json:"api_key_hash,omitempty"` // hex SHA-256 of the personal /v1 key
	APIKeyHint  string    `json:"api_key_hint,omitempty"` // last 4 characters
	APIKeyMade  time.Time `json:"api_key_made,omitzero"`
}

// Budget is a per-user monthly cap. Zero means no cap.
type Budget struct {
	MonthlyUSD    float64 `json:"monthly_usd"`
	MonthlyTokens int64   `json:"monthly_tokens"`
}

// RepoShare is one repo shared with a non-admin user.
type RepoShare struct {
	Path   string `json:"path"`   // absolute repo top level, as in /api/repos
	Accept bool   `json:"accept"` // may fast-forward main with Accept (default false: diff, save, zip only)
}
```

`Public` (what the API returns) is `User` without `Password` and `APIKeyHash`, plus computed
fields (see API contract).

`DATA/logins.json` (browser sign-ins):

```go
type Login struct {
	ID       string    `json:"id"`        // "l-" + 12 hex (shown in "Signed-in browsers")
	Hash     string    `json:"hash"`      // hex SHA-256 of the cookie token
	User     string    `json:"user"`      // user id
	Created  time.Time `json:"created"`
	LastSeen time.Time `json:"last_seen"` // written at most once a minute per login
	Expires  time.Time `json:"expires"`   // Created + users.session_max_days
	Addr     string    `json:"addr,omitempty"`
	Agent    string    `json:"agent,omitempty"` // User-Agent, clipped to 80 characters
}
```

A login token is `"cls-"` + 64 hex (32 random bytes). Only its SHA-256 is stored.

Personal /v1 API key: `"cuk-"` + 64 hex. Only its SHA-256 is stored, in the user record.

Per-user folder: `DATA/users/<id>/files/` (0700), made on first use. It is the working folder of
a non-admin user's chats and the target of their flows' file outputs.

Password hashing:

- PBKDF2-HMAC-SHA256, 600 000 iterations, 16-byte random salt per hash, 32-byte key.
- The iteration count is stored in the hash string. A login with an older count rehashes.
- Compare with `subtle.ConstantTimeCompare`.
- An unknown name still runs one PBKDF2 against a fixed dummy hash, so timing does not reveal
  which names exist.
- At most 2 hashes run at once (a semaphore; waiting honours the request context). This keeps a
  login flood from eating the CPU the models need.
- Password rules: 10 to 256 bytes, not equal to the name (case-insensitive). No other
  composition rules.

Config (add-only, `internal/config`, with schema metadata in group Safety):

```go
type UsersConfig struct {
	SessionIdleHours int `json:"session_idle_hours"` // default 336 (14 days): a login unused this long ends
	SessionMaxDays   int `json:"session_max_days"`   // default 30: a login ends this long after sign-in
	LockoutFailures  int `json:"lockout_failures"`   // default 5 failures per name ...
	LockoutMinutes   int `json:"lockout_minutes"`    // default 15 ... within this window locks the name for this long
}
```

`Config.Users UsersConfig json:"users"`. No other config changes; users never live in
`config.json`.

Store API (`internal/users`):

- `Open(dataDir string) (*Store, error)`, `OpenWith(dataDir, now)`.
- `HasUsers() bool`, `List() []User`, `Get(id) (User, bool)`, `ByName(name) (User, bool)`.
- `Add(name, display, role, password string) (User, error)`; the first user must be `admin`.
- `SetRole`, `SetDisabled`, `SetPassword(id, pw string, mustChange bool)`, `SetBudget`,
  `SetRepos`, `SetAgents`, `SetDisplay`, `Remove(id, to string)`.
- `Verify(name, password string) (User, error)`: `ErrBadLogin` for unknown name, wrong password
  or disabled user (one error, one message).
- `Owner(id string) string`: follows `Moved` (with a loop guard) to the current owner id.
  `Owner("")` is the legacy owner (the first admin), or "" when there are no users.
- `NewAPIKey(id) (string, error)`, `RevokeAPIKey(id)`, `VerifyAPIKey(tok) (User, bool)`.
- `Logins`: `Create(userID, addr, agent) (token string, Login)`, `Verify(tok) (Login, bool)`
  (expired or idle ones fail and are dropped), `Touch`, `List(userID)`, `Revoke(id)`,
  `RevokeUser(userID, except string) int`.
- `Lockout` (memory only): `Fail(name) (locked bool, retry time.Duration)`, `Locked(name)`,
  `Clear(name)`. Keys are the lowercased name, also for names that do not exist.
- `Can(role, cap string) bool` and `Caps(role) []string`: the role table above, in code.

### Migration of existing data and the admin token

- Nothing on disk is rewritten. Every owned object gets an `Owner` field (a user id).
  Objects from before users have `Owner == ""`.
- When the first user (always an admin) is created, the store sets `Moved[""] = <that id>`.
  From then on every `""` owner resolves to the first admin. This is the migration: all
  existing sessions, chats, flows, runs, inbox items, uploads, devices and MCP clients belong to
  the first admin.
- Deleting a user sets `Moved[<id>] = <target admin id>`. Their data is kept and reassigned
  (lazily, through `Owner()`), never listed as orphaned. The target is the admin who deletes,
  or `--to NAME` on the CLI. If the removed user was the legacy owner, `Moved[""]` follows too
  (`Owner()` follows chains).
- New objects always store the resolved owner id of their creator.
- The admin token stays. It acts as role `admin` with owner id `Owner("")` (the first admin;
  "" before any user exists). Audit actor stays `owner:admin`, user name "" (shown as "admin
  token"). `cerberus url`, `?token=` sign-in and `cerberus pair` keep working.
- Existing paired devices (`Device.User == ""`) act as the legacy owner, so as the first admin.

### Authentication flow

Principals (`internal/api/safety.go`): `principal` gains

```go
User   string // resolved owner id ("" only before users exist)
Name   string // user name for the audit log ("" for the admin token)
Role   string // admin | user | viewer | monitor | chat
Login  string // login id, for kindLogin
Must   bool   // the user must change their password first
```

and a new kind `kindLogin = "login"` (a password sign-in cookie). `kindUserKey = "user-key"`
is a personal /v1 key. The admin token is `kindAdmin`, role admin. A device takes the role of its
user. An agent token takes the owner of its session (see Agents). An MCP client takes its user.

`identify(tok)` order: admin token, `cls-` login, `cdv-` device, `cuk-` key (only on `/v1`).
A device or login whose user is disabled or removed does not identify.

Sign in:

1. `GET /api/auth` (public) tells the page whether users exist.
2. `POST /api/login` (public) with `{name, password}`:
   - Requires `X-Cerberus: 1`, `Content-Type: application/json`, and `Sec-Fetch-Site` empty,
     `same-origin` or `none` (login CSRF guard). Else 403.
   - Name locked: 429, `Retry-After`, `{error, code: "locked", retry_after}`. Same for names
     that do not exist.
   - Bad name, bad password or disabled user: 401 `{error: "Wrong user name or password.",
     code: "login"}`. It counts as one auth failure for the per-address limiter (5 per minute,
     then 429 for 10 minutes, as today) and one failure for the name's lockout.
   - Success: the name's failures clear, `LastLogin`/`LastAddr` update, a login is created and
     the cookie is set: `cerberus_token=cls-…; Path=/; HttpOnly; SameSite=Lax; Secure` (Secure
     on TLS only), `Max-Age` = seconds until `Expires`. Reply 200 with the `Me` object.
3. `POST /api/login` with `{token}` signs in with the admin token or a device token (the
   "Sign in with the admin token" path). Same CSRF guard and limiter. Success sets the cookie
   to that token, as `?token=` does today. Reply 200 `Me`. Bad token: 401 `{code: "login"}`.
4. `?token=` on a GET keeps working.

Cookie rules stay: a cookie write needs `X-Cerberus: 1`, and cookie requests to `/api` from
another site or a preview page get 403.

Sign out: `POST /api/logout` ends the caller's login (when it is a `cls-` cookie), clears the
cookie (`Max-Age=0`) and cuts the login's live streams. With a device or admin-token cookie it
only clears the cookie. 200 `{ok: true}`.

Must change: a login made with `MustChange` set gets 403 `{code: "must_change"}` on every route
except `GET /api/me`, `POST /api/me/password`, `POST /api/logout`, `GET /api/auth`, the UI
shell (`GET /{$}`, `/login`, assets, PWA files) and `GET /api/events` (which then carries only
pings). Device tokens of such a user are refused the same way.

Change own password: `POST /api/me/password {current, new}`. `current` is required unless the
user has `MustChange` set. Wrong `current` is 403 `{code: "login"}` and counts toward the
lockout. A rule failure is 400 `{code: "password"}`.
Success clears `MustChange`, ends every other login of the user, revokes the user's device
and MCP tokens (see Revocation), and re-issues the caller's cookie (new login). The admin token
has no password: 400 `{code: "no_user"}`.

Revocation (one helper, `server.revokeUser(id, keepLogin string) (cut int)`): ends all logins
of the user except `keepLogin`, revokes (deletes) every device and MCP client paired to the
user, and cuts all live requests of the user (`connTracker.cut("user:<id>")`). It runs on:
disable, role change, admin password reset, own password change (keeps the current login), and
delete. `admit` tracks a request under several keys: `user:<id>` for any principal with a
user, plus `device:<id>` or `login:<id>` (one `cut` on any key ends it). The cut generation
check (`epochKey`) covers all keys.

Session expiry: a login ends after `session_idle_hours` without use or `session_max_days` after
sign-in. Expired logins are pruned when the file is next written.

### Agents, devices and MCP clients

- Session agent tokens (`SessionAgentToken(id)`) act for the session's owner and only in the
  agent scope and `/v1`. `/api/agent/spawn` makes children owned by the parent's owner, with the
  owner's confinement and budget. `/api/agent/sessions/{id}/…` only reach sessions with the same
  owner (and, as today, only the caller's own children).
- `devices.Device` gains `User string json:"user,omitempty"` (owner id; "" = legacy owner).
  `PairScoped(name, scope, user string)`. `RevokeUser(user string) []Public`.
- Pairing (`POST /api/devices`): the admin token pairs for any user (`user` in the body, default
  legacy owner). A signed-in person on a login cookie pairs for themselves (`user` must be empty
  or their own id). A device token cannot pair (403), as today. Scope `mcp` needs the
  `sessions` capability (admin or user).
- An MCP client acts as its user. Sessions it starts are owned by that user, confined and
  budgeted like the user's own.
- Listing devices: admins see all (with the user name); others see their own. Revoking: admins
  any; others their own (else 404).

### Owner scoping

Rule: a user can see an object when `Owner(obj.Owner) == principal.User`, or the role is admin,
or the role is viewer and the object is a session-family object. Writes need the owner or
admin (with the chat exception above). An object the caller cannot see is 404 `{error: "not
found"}`, never 403, so ids of other people's work are not confirmed.

| Object | Owner field (JSON) | Stored in | Scoping |
|---|---|---|---|
| Session | `session.Info.Owner` (`"Owner"`); `OwnerName` filled on read | `DATA/sessions/<id>/meta.json` | list, get, stream, wall, feed, search, transcript, diff, files, archive, runs, flow, delegation, preview: owner, admin, viewer. Writes (actions, PATCH, bulk, DELETE, files writes): owner, admin. |
| Batch (fan-out) | `Batch.Owner` | `DATA/batches/<id>.json` | as sessions |
| Feed item | owner of its session | memory | as sessions |
| Upload staging | `owner` in `DATA/uploads/<id>/.owner` (0600) | `DATA/uploads/<id>/` | creator and admin; `TaskSpec.Staging` must belong to the caller |
| Chat | `Chat.Owner` (`"owner"`); `owner_name` on list | `DATA/chats/<id>.json` | owner; admin read, delete, stop |
| Chat permission asks | owner of the chat | memory | owner only (admins too see only their own) |
| Chat rules | owner of the chat | in the chat | owner |
| Flow | `Flow.Owner` (`"owner"`) | `DATA/flows/` | owner, admin |
| Flow run | `Run.Owner` (`"owner"`), set from the flow when the run starts | `DATA/flows/runs/` | owner, admin |
| Inbox item | `inbox.Item.Owner` (`"Owner"`) | inbox store | owner only. Items with Owner "" (system: downloads, alerts, serve) go to admins. Admins see their own plus system items. |
| Device / MCP client | `Device.User` (`"user"`) | `config/devices.json` | owner, admin |
| Usage record | `usage.Record.User` | usage JSONL | own sums for users; all for admin |
| Audit record | `audit.Record.User` (`"user"`, the name) | `DATA/audit.jsonl` | admin only |
| Model download | none (admin only) | | admin |
| Fine-tune examples and runs | none (admin only) | | admin |

`/api/events` filtering (per subscriber, decided per event):

- `session` and `usage`: only sessions the caller can see (role admin or viewer: all; user:
  own; monitor and chat: none). `removed` events too.
- `inbox`: only events for the caller's own items. The inbox keeps an unread count per owner;
  `Event.Unread` is the subscriber's own count. A "read all" applies to the caller's own items.
  `inbox.Event` gains `Owner`; the stream sends an event only when `Owner(ev.Owner)` matches, or
  `ev.Owner == ""` and the role is admin.
- `download`: admins only.
- `asks`: keys only for the caller's own chats. Each subscriber keeps its own signature.
- flow events: owner or admin. `flows.Event` gains `Owner`.
- `resync` and pings: everyone.
- Other streams check the object when they open: `/api/sessions/{id}/stream`,
  `/api/chats/{id}/stream`, `/api/wall/stream` (drops ids the caller cannot see), `/api/feed/
  stream` (filters per item), `/api/monitor/stream` (role check only).
- A role change or disable cuts the user's streams (Revocation); the page reconnects under the
  new rules.

Other filtered reads:

- `GET /api/status`: admin gets everything. user: own session counts, repos shared with them,
  allowed agents, models. viewer: all session counts, no repos, no agents. monitor and chat:
  version, host, uptime, voice, files limits (chat only), `flows_enabled: false`, no repos, no
  agents, no sessions.
- `GET /api/repos`: admin all; user only shared repos.
- `GET /api/search`, `GET /api/delegation`: owner filter for users.
- `GET /api/usage`: admin everything plus a `users` object (spend per user this month); user
  only records with their id; global budgets and plans only for admin.
- `GET /api/doctor` for viewer and monitor: `{counts: {ok, warn, fail}, checks: []}`.

### Confinement of non-admin sessions and chats

Agent sessions run as the Cerberus OS user. For a session whose owner's role is not admin:

- Workspace: `scratch`, or `repo` with a repo in `User.Repos`. `folder` workspaces and any other
  repo: 403 `{code: "workspace"}`. `Workspace "same"` children stay in the parent's workspace.
- Agent profile: only names in `User.Agents`; empty means the built-in agent (`cerberus`) only.
  Else 403 `{code: "agent"}`. The UI warns the admin that sharing a CLI profile (claude,
  opencode) shares the admin's own login of that tool, because its config folder is bound in.
- Sandbox: bwrap is forced, whatever `Sandbox.Default`, the profile or the repo say. If bwrap
  is missing or fails the probe, the start is refused: 503 `{code: "sandbox", error: "This
  server cannot sandbox sessions for other users: bubblewrap (bwrap) is missing. Ask the admin
  to install it."}`. Never a fallback to none.
- Sandbox spec (`sandbox.Spec`, new field `Confined bool`): `ReadOnlyHome` true; global
  `Sandbox.ExtraRW` dropped (only the allowed profile's `SandboxRW` stays); `ExtraRO` kept;
  HideHome as configured; Cerberus' data and config folders and `$XDG_RUNTIME_DIR` stay hidden
  (so `users.json`, other users' sessions and chats are unreachable). Network as configured.
- Accept on a shared repo needs `RepoShare.Accept`; else 403 `{code: "accept"}`. Diff, save to
  folder (only under the user's own folder) and zip stay.
- Checks from `.cerberus.json` run in the same sandbox.
- Host notifications (notify-send, ntfy, the command hook) fire only for sessions owned by an
  admin. Other owners get inbox items only.
- Session `Save` to folder for non-admins writes only under `DATA/users/<id>/files/`.

Chats of non-admin users:

- Role `chat`: `RunOptions.NoTools` always (plain chat; images in messages stay). The mode
  picker is hidden; the server ignores `mode`.
- Role `user`: the working folder is forced to `DATA/users/<id>/files/` (the `cwd` field is
  ignored). New `agent.RunOptions.Jail bool`: every read and write outside `Cwd` is refused (no
  "ask outside"). `run_command` runs inside bwrap with only `Cwd` writable and the data folders
  hidden (new `RunOptions.Wrap func(argv []string) []string`). Without bwrap the
  `run_command` tool is not offered, and the chat says so once.
- The `claude` chat backend is admin only (it runs the claude CLI with host-wide tools): 403
  `{code: "backend"}`.
- `/api/chats/atrium`, `/api/chats/claude` and their `continue` routes are admin only (they read
  the host account's own chat files).

Flows of non-admin users: session steps start sessions as the flow's owner with the rules
above. `output` to a file writes under the owner's folder. `output` to notify becomes an inbox
item for non-admin owners. Scheduled runs of a disabled user's flows are skipped with a note.

### Budgets

- `User.Budget{MonthlyUSD, MonthlyTokens}`; 0 means no cap. The month is the calendar month in
  local time.
- Spend this month = sum over usage records with `User == id` of `CostUSD`; tokens = prompt +
  completion tokens. `internal/usage` adds `ByUser(since time.Time) map[string]UserSum` with
  an incremental cache fed by new records.
- Attribution (`gateway`): every usage record gets `User`:
  - a personal key, cookie or device: that user;
  - a session agent token: the session's owner;
  - the admin token with `X-Cerberus-Session` naming a chat, session or flow run (the chat
    manager and flow host call the gateway this way): that object's owner;
  - `local_no_auth` and anything else: the legacy owner (admin).
- Enforcement points (all return the same error): `POST /api/sessions`, `POST /api/batches`,
  session actions that start a run (`resume`, `followup`, `checks` re-run, bulk resume),
  `POST /api/agent/spawn`, MCP starts, `POST /api/flows/{id}/run` and each flow session or prompt
  step, `POST /api/chats/{id}/messages`, `POST /api/quick`, and `/v1`.
  - API: 429 `{error: "Your monthly budget is used up (US$5.00 of US$5.00). Ask an admin to
    raise it.", code: "budget"}`.
  - `/v1`: 429 OpenAI error `{error: {message, type: "budget_exceeded", code: "user_budget"}}`.
- At 80 % an inbox item tells the user once per month.
- Global `Budgets` (per provider) still apply on top.
- Limit: tokens of subscription CLIs (claude, opencode) are not seen by the gateway and are not
  counted. The admin decides whether to share such a profile.
- Personal /v1 keys: roles admin and user. `local_no_auth` stays and acts as the admin only.

### Audit

- `audit.Record` gains `User string json:"user,omitempty"`: the acting user's name ("" for the
  admin token, system and CLI without a user). Agent records carry the session owner's name.
- New actor form for password sign-ins: `user:<name>`. Devices stay `owner:<device>` with
  `User` set. The admin token stays `owner:admin`.
- New records: `auth.login` (success, actor `user:<name>`), `auth.login_failed` (system, detail
  = the clipped name tried), `auth.locked`, `auth.logout`, `user.add`, `user.role`,
  `user.disable`, `user.enable`, `user.password` (never the password), `user.remove`,
  `user.apikey`, `user.revoked` (count of cut requests). Bodies are never stored.
- `GET /api/audit` gains `user=<name>` (exact). `cerberus audit --user NAME`.

### CLI (`cmd/cerberus/user.go`)

All `user` commands go through the daemon (admin bearer token, `X-Cerberus-Actor: cli`) when it
runs, else edit `DATA/users.json` directly and audit as `cli`.

- `cerberus user add NAME --role ROLE [--display TEXT] [--password-stdin | --generate]`.
  Without flags on a terminal it asks twice with no echo. `--generate` prints a 16-character
  password once and sets must-change. The first user must be `--role admin` (else exit 2 with
  a clear message).
- `cerberus user list [--json]`: name, role, status, last login, budget.
- `cerberus user passwd NAME [--password-stdin | --generate]`: resets and revokes.
- `cerberus user role NAME ROLE`.
- `cerberus user disable NAME` and `cerberus user enable NAME`.
- `cerberus user rm NAME [--to ADMIN] [--yes]`: the data goes to `--to` (default: the first
  enabled admin other than NAME). Refuses the last enabled admin.
- `cerberus user budget NAME [--usd N] [--tokens N]` and
  `cerberus user share NAME REPO [--accept] | --unshare REPO`.
- The last enabled admin can never be removed, disabled or demoted (exit 2; API 409
  `{code: "last_admin"}`).
- `cerberus help user` documents all of it.

### Doctor checks (new, in `internal/api/doctor`)

- `users`: warn when no admin user exists ("Run: cerberus user add NAME --role admin"). Ok with
  the count per role otherwise.
- `users_tls`: fail when users exist and any listen address is not loopback and has no TLS
  (passwords would cross the network in clear). Fix: `tls_cert`/`tls_key` or Tailscale serve.
- `linger` (Linux): warn when the service unit is installed and
  `/var/lib/systemd/linger/<user>` does not exist (the daemon stops at logout). Fix:
  `loginctl enable-linger $USER`. Skipped elsewhere.
- `users_sandbox`: fail when a user with role `user` exists and bwrap is not usable.
- `local_no_auth`: now warns too when users exist (any local process acts as admin on /v1).
- `modes`: also checks `users.json` and `logins.json` are 0600.

### Not found (404) behaviour

- Hash routes: `parseRoute` returns `{view: 'notfound', path}` for an unknown first segment.
  Empty hash goes to the role's home. The Not found view says "Nothing here", shows the hash,
  and offers a button to the role's home. A view the role cannot use also shows this view.
- Server, unknown non-API paths: a catch-all pattern `/` serves `web/dist/404.html` with status
  404 (`Content-Type: text/html; charset=utf-8`, `Cache-Control: no-cache`) for GET and HEAD,
  and `404 {error: "not found"}` JSON for other methods. It is public: `withAuth` treats a
  request the mux would route to `/` as public (`s.mux.Handler(r)` pattern check), so a signed-
  out visitor gets the 404 page, not a redirect. When `404.html` is missing from the build the
  server answers a minimal built-in HTML page.
- `404.html`: self-contained, no data, works without JavaScript. It loads only same-origin
  public files: `assets/theme-boot.js`, `assets/fonts/carino-fonts.css`,
  `assets/carino-ui.css`, `assets/carino-theme.css`, `assets/app.css`. Absolute paths
  (`/assets/…`) because it is served at any depth. Title "Not found · Cerberus", a short text and
  a link to `/`.
- Unknown `/api/…`: stays JSON 404 after auth (`{error: "unknown endpoint …"}`); signed-out it
  stays 401.
- Unknown `/v1/…`: unchanged (the gateway's OpenAI-style errors).

### Admin panel (web, Settings → Users)

`#/settings/users`, admin only; a panel like Security. Desktop: a table. Phone: one card per
user.

- Columns: name (and display), role, status (Active, Disabled, Must change password, Locked),
  last login (relative time, address on hover), sessions count, chats count, spend this month
  (US$ and tokens, with the budget as "of US$X"), actions.
- Add user: a dialog with name, display, role (select with one-line role help), password
  (type it, or "Generate"). A generated password shows once with Copy, and the user must change
  it at first sign-in.
- Row actions: change role (select, confirm), reset password (generate or type; shows the new
  one once), disable or enable, edit budget (USD and tokens, 0 = none), shared repos (pick from
  `/api/repos`, with an "may accept into main" switch each), allowed agents (checkboxes, with
  the warning for CLI profiles), revoke API key, delete.
- Delete: a dialog that names what happens ("Their 12 sessions, 30 chats and 2 flows move to
  you. Nothing is deleted.") and asks to type the user name. The reply's counts show in a toast.
- The last enabled admin's role select, disable and delete are disabled with a hint.
- Audit log (Settings → Security): a user filter select (from `/api/users`, plus "admin token"
  and "system").
- Account (`#/account`, all roles): name, role, change password, signed-in browsers (this one
  marked) with sign out, own devices (pair, revoke), personal API key (admin, user: create
  shows it once; revoke), budget and spend this month. Sign out.
- The navbar actions block gets an account button (initial or person icon, name on desktop):
  Account, Sign out. Phones: in the More sheet too.
- Sessions list and wall: admins and viewers see an owner chip on each row and an owner filter.
- Viewer: no New task, no bulk bar, no approve buttons, no terminal input, no session actions;
  the terminal shows "Read only".

### Login page (web)

`#/login` (also served at `/login`). One card.

- Fields: User name (`autocomplete=username`), Password (`autocomplete=current-password`, show
  toggle), Sign in button.
- Errors: one generic line, "Wrong user name or password." For 429: "Too many attempts. Try
  again in N minutes." The button is disabled while the request runs.
- "Sign in with the admin token" is a disclosure under the form with the token field. When
  `GET /api/auth` says `has_users: false`, the disclosure is open and the card says "No
  accounts yet. Sign in with the admin token to create the first admin."
- After a token sign-in with no users (`me.bootstrap`), the first-run screen `#/first-admin`
  asks for name, display and password (twice), creates the admin (`POST /api/users`), signs in
  as that user (`POST /api/login`) and opens the role home (then the existing welcome, if
  `init_done` is false).
- `me.must_change`: `#/password`, the forced change: new password twice. The current password
  is not asked (the server takes the fresh login as proof while `must_change` is set).
- No navbar tabs on login, first-admin and forced password screens.

### docs/USERS.md (new) outline

1. What multi-user Cerberus is, and what it is not (one OS user, bwrap confinement, not a
   hostile-tenant system).
2. Run it as a service: `cerberus install-service`, `loginctl enable-linger $USER`, check with
   `cerberus doctor`.
3. Listen on the LAN or tailnet: `server.extra_listen`, HTTPS (`tls_cert`/`tls_key`,
   `tailscale cert`, Tailscale serve), `allowed_hosts`, `public_url`. Never the public
   internet.
4. First admin: `cerberus user add NAME --role admin`, or the web first-run with the admin
   token.
5. Adding users: CLI and Settings → Users; generated passwords; must change.
6. Roles table (the one above, in plain words).
7. Sharing repos and agent profiles; what bwrap hides; why folder workspaces are admin only.
8. Budgets and personal API keys; what is not counted.
9. Devices and MCP clients per user.
10. Sign-in, lockout, sign out, password change, revocation, forgotten passwords
    (`cerberus user passwd`).
11. The admin token as break-glass; rotation.
12. Backups: `DATA/users.json`, `DATA/logins.json` (optional), `config/devices.json`,
    `config.json`, `DATA/sessions`, `DATA/chats`, `DATA/flows`, `DATA/users/`, the vault; stop
    the service or copy with care; restore and file modes.
13. Removing a user: where their data goes.
14. Audit log per user.

### Route → role class table

Classes (allowed roles; "own" means owner scoping applies):

- `P` public. `NF` public catch-all 404.
- `ANY` every signed-in person (all five roles, the admin token, devices).
- `ADM` admin only.
- `WORK` admin, user (own).
- `WATCH` admin, user (own), viewer (all); GET only.
- `MON` admin, viewer, monitor.
- `CHAT` admin, user, chat (own).
- `GW` `/v1`: admin, user (cookie, device or personal key), agent tokens; chat, viewer, monitor
  refused.
- `AGENT` the agent scope (session agent tokens, MCP clients), acting for the owner.

A role refused by the class gets 403 `{error: "Your role (viewer) cannot do this.", code:
"role"}`. Implementation: `internal/api/roles.go` holds `routeRoles map[string]roleClass` keyed
by mux pattern. `routeMux.HandleFunc/Handle` wrap every handler with the class check and panic
at startup for a pattern without a class (fail closed).

| Pattern | Class | Notes |
|---|---|---|
| `GET /healthz` | P | |
| `GET /login` | P | |
| `GET /assets/` | P | |
| `GET /manifest.webmanifest` | P | |
| `GET /sw.js` | P | |
| `GET /carino-clock.js` | P | |
| `GET /{$}` | ANY | signed out: 302 /login |
| `/` (new) | NF | 404.html |
| `GET /preview/{id}` | WATCH | capability URLs unchanged |
| `GET /preview/{id}/{path...}` | WATCH | |
| `/v1/` | GW | budget, attribution |
| `GET /api/status` | ANY | filtered per role |
| `GET /api/models` | WORK | |
| `POST /api/models/{rest...}` | ADM | |
| `GET /api/usage` | WORK | own for user |
| `POST /api/usage/watch` | WORK | |
| `GET /api/repos` | WORK | user: shared repos |
| `GET /api/repos/find` | ADM | |
| `GET /api/secrets` | ADM | |
| `PUT /api/secrets/{name}` | ADM | |
| `DELETE /api/secrets/{name}` | ADM | |
| `GET /api/search` | WATCH | |
| `POST /api/prune` | ADM | |
| `GET /api/sessions/{id}/transcript` | WATCH | |
| `GET /api/batches` | WATCH | |
| `POST /api/batches` | WORK | budget, confinement |
| `GET /api/batches/{id}` | WATCH | |
| `POST /api/batches/{id}/{action}` | WORK | |
| `GET /api/voice` | CHAT | |
| `POST /api/voice/transcribe` | CHAT | |
| `GET /api/sessions` | WATCH | |
| `POST /api/sessions` | WORK | budget, confinement |
| `GET /api/sessions/{id}` | WATCH | |
| `GET /api/sessions/{id}/diff` | WATCH | |
| `GET /api/sessions/{id}/stream` | WATCH | |
| `GET /api/sessions/{id}/archive` | WATCH | |
| `GET /api/sessions/{id}/preview` | WATCH | work previews (2026-11-03) |
| `POST /api/sessions/{id}/preview/start` | WORK | Run preview; audited |
| `POST /api/sessions/{id}/preview/stop` | WORK | audited |
| `GET /api/fs` | ADM | host folder browser |
| `POST /api/sessions/{id}/{action}` | WORK | accept: RepoShare.Accept |
| `DELETE /api/sessions/{id}` | WORK | |
| `GET /api/events` | ANY | filtered per subscriber |
| `GET /api/monitor` | MON | |
| `GET /api/monitor/processes` | MON | |
| `GET /api/monitor/series` | MON | |
| `GET /api/monitor/stream` | MON | |
| `POST /api/monitor/processes/{pid}/{action}` | ADM | |
| `/api/` | ANY | JSON 404 |
| `PATCH /api/sessions/{id}` | WORK | |
| `POST /api/sessions/bulk` | WORK | ids filtered to own; others reported as not found |
| `GET /api/feed` | WATCH | |
| `GET /api/feed/stream` | WATCH | |
| `GET /api/wall/stream` | WATCH | |
| `POST /api/agent/spawn` | AGENT | owner, budget, confinement |
| `GET /api/agent/children` | AGENT | |
| `GET /api/agent/models` | AGENT | |
| `POST /api/agent/ask` | AGENT | |
| `POST /api/agent/rate` | AGENT | |
| `GET /api/agent/sessions/{id}/tail` | AGENT | same owner |
| `POST /api/agent/sessions/{id}/send` | AGENT | same owner |
| `GET /api/agent/sessions/{id}/wait` | AGENT | same owner |
| `POST /api/agent/sessions/{id}/stop` | AGENT | same owner |
| `GET /api/agent/files` | AGENT | |
| `GET /api/agent/files/raw` | AGENT | |
| `GET /api/agent/runs` | AGENT | |
| `GET /api/agent/runs/{name}` | AGENT | |
| `/api/agent/` | AGENT | |
| `POST /api/sessions/{id}/files` | WORK | |
| `GET /api/sessions/{id}/files` | WATCH | |
| `GET /api/sessions/{id}/files/raw` | WATCH | |
| `DELETE /api/sessions/{id}/files` | WORK | |
| `POST /api/sessions/{id}/files/mkdir` | WORK | |
| `POST /api/uploads` | WORK | staging owner |
| `GET /api/uploads/{staging}` | WORK | |
| `DELETE /api/uploads/{staging}` | WORK | |
| `GET /api/audit` | ADM | `user=` filter |
| `GET /api/devices` | ANY | own; admin all |
| `POST /api/devices` | ANY | pair for self (login cookie) or anyone (admin token); `mcp` scope needs WORK roles |
| `DELETE /api/devices/{id}` | ANY | own; admin all |
| `POST /api/token/rotate` | ADM | |
| `GET /api/doctor` | MON | counts only for viewer, monitor |
| `GET /api/config` | ADM | |
| `GET /api/config/schema` | ADM | |
| `PATCH /api/config` | ADM | |
| `POST /api/config/restart` | ADM | |
| `POST /api/config/init-done` | ADM | |
| `GET /api/chats` | CHAT | admin: `?all=1` |
| `POST /api/chats` | CHAT | cwd forced for non-admin |
| `GET /api/chats/models` | CHAT | |
| `GET /api/chats/search` | CHAT | own |
| `GET /api/chats/atrium` | ADM | |
| `POST /api/chats/atrium/{id}/continue` | ADM | |
| `GET /api/chats/claude` | ADM | |
| `POST /api/chats/claude/{id}/continue` | ADM | |
| `GET /api/chats/asks` | CHAT | own chats only |
| `GET /api/chats/rules` | CHAT | own |
| `DELETE /api/chats/rules` | CHAT | own |
| `GET /api/chats/{id}` | CHAT | owner; admin read |
| `PATCH /api/chats/{id}` | CHAT | owner |
| `DELETE /api/chats/{id}` | CHAT | owner; admin |
| `POST /api/chats/{id}/messages` | CHAT | owner only; budget |
| `GET /api/chats/{id}/stream` | CHAT | owner; admin read |
| `POST /api/chats/{id}/answer` | CHAT | owner only |
| `POST /api/chats/{id}/stop` | CHAT | owner; admin |
| `GET /api/chats/{id}/rules` | CHAT | owner |
| `DELETE /api/chats/{id}/rules` | CHAT | owner |
| `POST /api/quick` | CHAT | budget; no tools |
| `GET /api/inbox` | ANY | own |
| `POST /api/inbox/read` | ANY | own |
| `DELETE /api/inbox/{id}` | ANY | own |
| `GET /api/sessions/{id}/runs` | WATCH | |
| `GET /api/sessions/{id}/runs/{name}` | WATCH | |
| `GET /api/downloads` | ADM | |
| `POST /api/downloads` | ADM | |
| `DELETE /api/downloads/{id}` | ADM | |
| `GET /api/leaderboard` | WATCH | model stats, not personal |
| `GET /api/delegation` | WATCH | own for user |
| `GET /api/sessions/{id}/delegation` | WATCH | |
| `GET /api/finetune` | ADM | |
| `GET /api/finetune/export` | ADM | |
| `GET /api/finetune/examples` | ADM | |
| `GET /api/finetune/examples/{id}` | ADM | |
| `POST /api/finetune/examples/{id}/rate` | ADM | |
| `POST /api/finetune/examples/{id}/exclude` | ADM | |
| `POST /api/finetune/train` | ADM | |
| `GET /api/finetune/runs` | ADM | |
| `GET /api/sessions/{id}/flow` | WATCH | |
| `GET /api/chats/{id}/flow` | CHAT | owner; admin read |
| `GET /api/flows` | WORK | |
| `POST /api/flows` | WORK | |
| `GET /api/flows/templates` | WORK | |
| `POST /api/flows/validate` | WORK | |
| `POST /api/flows/test` | WORK | budget |
| `GET /api/flows/runs` | WORK | |
| `GET /api/flows/runs/{run}` | WORK | |
| `POST /api/flows/runs/{run}/cancel` | WORK | |
| `POST /api/flows/runs/{run}/approve` | WORK | |
| `GET /api/flows/runs/{run}/flow` | WORK | |
| `GET /api/flows/{id}` | WORK | |
| `PUT /api/flows/{id}` | WORK | |
| `DELETE /api/flows/{id}` | WORK | |
| `POST /api/flows/{id}/run` | WORK | budget |
| `GET /api/config/export` | ADM | |
| `POST /api/config/import` | ADM | |
| `POST /api/config/models/forget` | ADM | |
| `GET /api/auth` (new) | P | |
| `POST /api/login` (new) | P | CSRF guard, limiter, lockout |
| `POST /api/logout` (new) | ANY | allowed under must-change |
| `GET /api/me` (new) | ANY | allowed under must-change |
| `POST /api/me/password` (new) | ANY | allowed under must-change |
| `GET /api/me/logins` (new) | ANY | own browsers |
| `DELETE /api/me/logins/{id}` (new) | ANY | own |
| `POST /api/me/apikey` (new) | WORK | |
| `DELETE /api/me/apikey` (new) | WORK | |
| `GET /api/users` (new) | ADM | |
| `POST /api/users` (new) | ADM | admin token allowed with no users (first admin) |
| `GET /api/users/{id}` (new) | ADM | |
| `PATCH /api/users/{id}` (new) | ADM | |
| `POST /api/users/{id}/password` (new) | ADM | |
| `DELETE /api/users/{id}` (new) | ADM | |
| `DELETE /api/users/{id}/apikey` (new) | ADM | |
| `GET /api/permits` (saved permissions) | WORK | own rules |
| `POST /api/permits` | WORK | own; validated |
| `DELETE /api/permits/{id}` | WORK | own (404 otherwise) |

Before any user exists, every class but `P`, `NF` and `AGENT` behaves as today for the admin
token and devices (they are role admin).

### Test plan

Go (`go test ./...`, `-race` on the touched packages, `GOOS=darwin` and `windows` builds):

- `internal/users`: hash format and round trip; 600 000 iterations; rehash of an older count;
  constant-time path for unknown names (the dummy hash runs); password rules; name rules and
  uniqueness (case-insensitive); first user must be admin; last-admin guards; `Moved` and
  `Owner()` chains with a loop guard; reload on outside change; 0600 modes; logins create,
  verify, idle and absolute expiry, prune, revoke per user; lockout per name (unknown names
  too), window and clear; API keys; `Can`/`Caps` against the role table.
- `internal/api`:
  - `routes_matrix_test.go`: `routeSpec` gets the class; `TestRouteTableClassified` also checks
    the table equals `routeRoles`. `TestRouteMatrix` runs every pattern and method for: no
    credential; admin token; a login cookie and a paired device for each of the five roles;
    session agent token; shared agent token; MCP client; personal API key. Admitted or 403
    `code: role` exactly per class; 401 without a credential; CSRF 403 on cookie writes without
    the header; must-change logins refused except their allow-list. `TestRouteMatrixAudit`
    checks `User` on records.
  - Ownership: user A cannot get, stream, act on, delete, search or bulk B's sessions, chats,
    flows, runs, uploads, inbox items or devices (404); viewer reads all sessions and is refused
    every write; admin reads everything and cannot post into B's chat.
  - `/api/events`: two users and a viewer subscribed; only allowed `session`, `usage`, `inbox`,
    `asks`, flow and `download` events arrive; per-owner unread counts.
  - Login: success sets the cookie; wrong password, unknown name and disabled user give the
    same body; lockout after 5; per-address limiter; CSRF guard; token sign-in; logout;
    password change ends other logins and devices; disable, role change and reset cut a live SSE
    stream at once.
  - Confinement: non-admin session start without bwrap → 503 `sandbox`; folder workspace → 403;
    unshared repo → 403; unallowed agent → 403; argv has the forced bwrap with `Confined`;
    accept needs `RepoShare.Accept`.
  - Budgets: over-budget start, chat send, quick, flow run and `/v1` refused with the right
    codes; attribution of chat and session gateway calls to the owner.
  - 404: unknown path → 404 HTML for signed-out and signed-in; HEAD; POST → JSON; unknown /api
    → JSON 404 (401 signed-out); `/v1/x` unchanged.
  - Status, repos, usage and doctor filtering per role.
- `internal/chat`: owner on create; `AskKeys` per owner; NoTools for role chat; Jail and Wrap for
  role user; claude backend refused for non-admins.
- `internal/agent`: `Jail` refuses reads and writes outside Cwd; `Wrap` wraps `run_command`.
- `internal/session`, `internal/flows`, `internal/inbox`, `internal/usage`, `internal/audit`,
  `internal/sandbox`, `internal/api/devices`, `internal/api/doctor`: their new fields, filters
  and checks, each with table tests.
- `cmd/cerberus`: `user` subcommands with the daemon down (files) and up (fake server), first
  user admin, `--password-stdin`, `--generate`, last-admin refusal, `rm --to`, `audit --user`,
  help text.

Web (`node --test web/test`):

- `web/test/users.test.mjs`: caps per role, tab visibility per role, route guard and role home,
  `parseRoute` → `notfound`, login error mapping (401, 429 with minutes, 403 must_change),
  admin panel helpers (row status, spend text, last-admin guard, delete summary text).
- Existing tests keep passing; `python3 web/tools/build_sw.py` after every change to
  `web/dist`, and `go test ./web` passes.

Browser (Playwright, demo daemon only, ports 7397/7398, one Chromium at a time): widths 360, 390
and 1280; Gold and Colourful themes; no horizontal scroll; no console errors. Screens: login
(empty, error, locked), first admin, forced password change, each role's home and tab set,
Settings → Users (list, add, reset, delete dialog), Account, viewer session page (read only),
Not found view, `/nope` 404 page signed out and in.

### API contract (what the web build codes against)

General:

- Errors are `{error: string, code?: string}`. Codes: `role` (403, the role cannot use the
  route), `must_change` (403), `login` (401 bad sign-in, or 403 wrong current password),
  `locked` (429, also `retry_after` seconds and a `Retry-After` header), `password` (400 rule
  failure), `name` (400 bad or taken name; 409 when taken), `last_admin` (409), `self` (409, an
  admin cannot delete or disable themselves), `no_user` (400, the admin token has no account),
  `budget` (429), `sandbox` (503), `workspace`, `agent`, `accept`, `backend` (403).
- Objects the caller cannot see: 404 `{error: "not found"}`.
- Signed out: 401 (as today), the UI goes to `#/login`.
- Cookie writes keep needing `X-Cerberus: 1` (api.js already sends it).
- Times are RFC 3339 strings. Money is US dollars as numbers.

`GET /api/auth` (public) → 200

```json
{"has_users": true, "token_login": true}
```

`POST /api/login` (public; `X-Cerberus: 1`, JSON) body `{"name": "ana", "password": "…"}` or
`{"token": "…"}` → 200 `Me` + `Set-Cookie`. 401 `{error: "Wrong user name or password.", code:
"login"}`. 429 `{error: "Too many attempts. Try again in 15 minutes.", code: "locked",
retry_after: 900}`. 400 bad body. 403 cross-site.

`POST /api/logout` → 200 `{"ok": true}` and the cookie cleared.

`GET /api/me` → 200 `Me`:

```json
{
  "kind": "login",
  "user": {"id": "u-3f9a1c2b7d01", "name": "ana", "display": "Ana", "role": "user",
           "must_change": false, "created": "…", "last_login": "…"},
  "role": "user",
  "caps": ["chat", "chat.tools", "sessions", "flows", "models.read", "usage", "gateway", "voice", "devices"],
  "home": "#/chat",
  "has_users": true,
  "bootstrap": false,
  "budget": {"monthly_usd": 5, "monthly_tokens": 0, "spent_usd": 1.25, "spent_tokens": 48211},
  "api_key": {"set": true, "hint": "9f2c", "made": "…"},
  "device": ""
}
```

- `kind`: `login` | `device` | `admin-token`. `user` is null for the admin token (role
  `admin`, caps all). `device` is the device id for `kind: device`.
- `bootstrap` is true when `kind` is `admin-token` and `has_users` is false: show
  `#/first-admin`.
- Cap ids: `chat`, `chat.tools`, `chat.import`, `sessions`, `sessions.watch_all`,
  `sessions.write`, `flows`, `monitor`, `monitor.signal`, `doctor`, `doctor.counts`,
  `models.read`, `models.manage`, `usage`, `usage.all`, `gateway`, `voice`, `devices`,
  `devices.all`, `settings`, `secrets`, `users`, `audit`, `finetune`, `host`.

`POST /api/me/password` body `{"current": "…", "new": "…"}` → 200 `{"ok": true}` (new cookie
set). 403 `login`, 400 `password`, 400 `no_user`.

`GET /api/me/logins` → 200 `{"logins": [{"id": "l-…", "created": "…", "last_seen": "…",
"expires": "…", "addr": "192.168.1.20", "agent": "Mozilla/5.0 …", "current": true}]}`.
`DELETE /api/me/logins/{id}` → 200 `{"ok": true}`; 404.

`POST /api/me/apikey` → 200 `{"key": "cuk-…", "hint": "9f2c"}` (shown once; replaces the old
key). `DELETE /api/me/apikey` → 200 `{"ok": true}`.

`UserPublic` (admin panel):

```json
{
  "id": "u-3f9a1c2b7d01", "name": "ana", "display": "Ana", "role": "user",
  "disabled": false, "must_change": false, "locked": false,
  "created": "…", "updated": "…", "last_login": "…", "last_addr": "192.168.1.20",
  "budget": {"monthly_usd": 5, "monthly_tokens": 0},
  "spent_usd": 1.25, "spent_tokens": 48211,
  "sessions": 12, "chats": 30, "flows": 2, "devices": 1,
  "repos": [{"path": "/home/carino/Github/Topo", "accept": false}],
  "agents": [],
  "api_key": {"set": true, "hint": "9f2c", "made": "…"},
  "legacy_owner": false
}
```

`legacy_owner` is true for the user that `Moved[""]` resolves to.

`GET /api/users` → 200 `{"users": [UserPublic…], "roles": ["admin","user","viewer","monitor","chat"],
"agents": ["cerberus","claude",…]}` (`agents`: profile names the admin can allow).

`POST /api/users` body `{"name": "ana", "display": "Ana", "role": "user", "password": "…"}`
(`password` optional: omitted → generated, `must_change` true) → 201 `{"user": UserPublic,
"password": "…"}` (`password` only when generated). 400 `name`/`password`/bad role; 409
`name` (taken). With no users the role must be `admin` (400 otherwise); the admin token is the
only credential that exists then.

`GET /api/users/{id}` → 200 `UserPublic`; 404.

`PATCH /api/users/{id}` body, any subset: `{"display": "…", "role": "viewer", "disabled":
true, "budget": {"monthly_usd": 10, "monthly_tokens": 0}, "repos": [{"path": "…", "accept":
true}], "agents": ["cerberus"]}` → 200 `{"user": UserPublic, "cut": 3}` (`cut`: live requests
ended by revocation; role and disabled changes revoke). 400 bad value (`repos` must be in
`/api/repos`, `agents` must exist); 409 `last_admin`; 409 `self` (an admin disabling or
demoting themselves); 404.

`POST /api/users/{id}/password` body `{"password": "…"}` or `{}` (generate) → 200 `{"ok":
true, "password": "…"(only generated), "cut": 2}`. Sets `must_change`, clears the lockout,
revokes. 400 `password`.

`DELETE /api/users/{id}` body `{"to": "u-…"}` optional (default the caller; the admin token
defaults to the legacy owner) → 200 `{"ok": true, "reassigned_to": "u-…", "moved": {"sessions":
12, "chats": 30, "flows": 2, "runs": 7, "inbox": 4}, "devices_revoked": 1, "cut": 2}`. 409
`last_admin`, 409 `self`, 400 when `to` is not an enabled admin, 404.

`DELETE /api/users/{id}/apikey` → 200 `{"ok": true}`.

Changes to existing replies (add-only fields):

- Session `Info`: `"Owner": "u-…"`, `"OwnerName": "ana"` (on every session object: list, get,
  `session` events, wall). Query `GET /api/sessions?owner=<id>` filters (admin, viewer).
- Chat list items and chat objects: `"owner"`, `"owner_name"`. `GET /api/chats?all=1` (admin)
  lists every user's chats.
- Flow and run objects: `"owner"`, `"owner_name"`. `GET /api/flows?all=1` and
  `GET /api/flows/runs?all=1` (admin; default is own).
- Inbox items: `"Owner"`; `Unread` is the caller's own count.
- Devices: `"user"`, `"user_name"`; `POST /api/devices` takes `"user"` (admin token only).
  `GET /api/devices` → `{devices, current}` as today (own, or all for admins).
- Audit: records have `"user"`; `GET /api/audit?user=<name>`; `user=-` means records with no
  user (admin token, system).
- `GET /api/usage`: for admins a new `"users": [{"id","name","spent_usd","spent_tokens",
  "budget"}]`; for users only their own records and `spend`, no `budgets`/`plans`.
- `GET /api/status`: fields absent for a role are their empty values (`repos: []`,
  `agents: []`, `sessions: {}`, `models: []`).
- `GET /api/doctor` for viewer and monitor: `{"counts": {"ok": 14, "warn": 2, "fail": 0},
  "checks": []}`. Admins get the full report plus `counts`.
- `GET /api/events`: same event names; filtered per subscriber. `asks` keys are own only.
- Unknown non-API path: 404 with `text/html` (the 404 page).

### Build split

Two builds with disjoint files. Neither edits the other's files.

BACKEND: `internal/**`, `cmd/**`, `docs/USERS.md` (new), `docs/SECURITY.md` (actors and
roles), and the as-built notes at the end of this file.

- `internal/users` (new): `users.go` (types, roles, caps table, name rules), `store.go`
  (users.json, reload, Moved/Owner), `password.go` (PBKDF2, semaphore, dummy hash),
  `logins.go` (logins.json), `lockout.go`, `apikey.go`, tests.
- `internal/config`: `UsersConfig` + defaults + schema metadata + validation.
- `internal/api`: `roles.go` (route classes, enforced in `routeMux`), `login.go`
  (`/api/auth`, `/api/login`, `/api/logout`), `me.go`, `users.go` (admin routes),
  `owner.go` (scoping helpers), changes in `auth.go` (identify order, must-change gate,
  catch-all 404 as public), `safety.go` (principal fields, multi-key revocation), `sessions.go`
  (events filtering, owner on create, confinement, budgets), `control.go`, `files.go`,
  `chat.go`, `flows.go`, `extras.go`, `history.go`, `status.go`, `spend.go`, `security.go`
  (devices per user, audit filter, doctor counts), `ui.go` (`/` catch-all, 404.html with a
  built-in fallback), `agentscope.go` (owner), `routes_matrix_test.go` (classes per role).
- `internal/api/devices`: `User` field, `PairScoped(name, scope, user)`, `RevokeUser`.
- `internal/api/doctor`: `users`, `users_tls`, `linger`, `users_sandbox`; `local_no_auth`
  and `modes` extended.
- `internal/session`: `Info.Owner`/`OwnerName`, `TaskSpec.Owner` and `TaskSpec.Confine`
  (forced bwrap, allowed repos and agents, accept flag), batch owner, search owner filter,
  host notify only for admin owners.
- `internal/sandbox`: `Spec.Confined`.
- `internal/chat`: `Chat.Owner`, per-owner asks, NoTools/Jail/Wrap per role, forced cwd,
  claude backend admin only, owner-attributed gateway calls.
- `internal/agent`: `RunOptions.Jail`, `RunOptions.Wrap`.
- `internal/flows`: `Flow.Owner`, `Run.Owner`, `Event.Owner`, owner for steps and outputs.
- `internal/inbox`: `Item.Owner`, per-owner unread, `Event.Owner`.
- `internal/usage`: `Record.User`, `ByUser`.
- `internal/gateway`: attribution, per-user budget hook, personal keys.
- `internal/audit`: `Record.User`, user filter.
- `internal/files`: staging owner.
- `cmd/cerberus`: `user.go` (+ tests), help, `audit --user`, doctor wiring, `serve` opens the
  users store.

WEB: `web/dist/**`, `web/test/**`, `web/tools/**`.

- `web/dist/404.html` (new).
- `assets/js/api.js`: codes `must_change` (→ `#/password`), `role` (toast), keep 401 → login.
- `assets/js/store.js`: load `GET /api/me` before starting events; `me`, `can(cap)`, role home;
  reload `me` after login, logout, password change and on every reconnect.
- `assets/js/app.js`: tabs and More sheet filtered by caps; route guard (not allowed → Not
  found); no events or feed on login, first-admin and password screens; `initFeed` and
  `maybeWelcome` only for admins.
- `assets/js/util.js`: `parseRoute` adds `notfound`, `account`, `first-admin`, `password`,
  `settings/users`.
- `assets/js/users.js` (new, pure helpers for tests): caps, role labels and help, role home,
  status of a user row, spend text, delete summary.
- Views: `views/login.js` (rewrite), `views/firstadmin.js`, `views/password.js`,
  `views/account.js`, `views/users.js` (the Settings → Users panel), `views/notfound.js`,
  account button in the navbar actions block (before `carino-navbar.js` moves it, like the
  bell), `views/settings.js` (Users panel and nav entry for admins), `views/security.js` (audit
  user filter; devices with user names for admins; own devices otherwise), `views/sessions.js`,
  `views/wall.js`, `views/session.js` (owner chips and filter; read-only for viewer),
  `views/chat.js` (no mode picker or cwd for `chat`; no cwd for `user`; no default-model PATCH
  unless `settings`; admin "All users" filter), `views/bell.js` (no approvals for viewer; no
  downloads unless admin), `views/monitor.js` (no process actions unless `monitor.signal`;
  doctor counts card from `counts`), `views/welcome.js` (admins only).
- CSS in the existing sheets (or a new `assets/users.css` linked from `index.html`).
- `web/test/users.test.mjs` (new); fixes to existing tests that assume one owner.
- Run `python3 web/tools/build_sw.py` after every change under `web/dist`.

Order: BACKEND first (the contract above is fixed, so WEB can start against it with fixtures
in parallel); then a joint browser pass on the demo daemon.

### As built: users and roles, backend (2026-11-03)

Built as specified above, with these deviations and details:

- `internal/users`: personal API keys live in `store.go` (no separate `apikey.go`). `SetCostForTest`
  lowers the PBKDF2 count for other packages' tests only. A sign-in that fails on the attempt that
  locks the name still answers 401; the next attempts answer 429 `locked`.
- Upload staging owner: the sidecar is `DATA/uploads/<id>.owner` (beside the folder, 0600), not
  `DATA/uploads/<id>/.owner`, because everything inside the staging folder moves into the session that
  takes it. `Sweep` removes sidecars whose folder is gone.
- Attribution (`/v1`): `withAuth` deletes any incoming `X-Cerberus-User` and the gateway handler sets it
  from the principal (`gatewayOwner`); `gateway/proxy.go` copies it into `usage.Record.User`. Daemon
  callers that use the admin token name their owner either by `X-Cerberus-Session` (chat, session or
  flow run id) or by `X-Cerberus-Owner` (honoured for the admin token only; `chat.WithOwner` sets it for
  quick answers). The flow host calls the gateway in-process and sets `X-Cerberus-User` itself.
- `GET /api/usage` for non-admins: `{recent, spend: {month_usd, month_tokens}}` (own records only; no
  `agg_24h`, `budgets` or `plans`). `GET /api/delegation` for role user keeps only their sessions'
  rows and totals; `by_model` is empty (it mixes everyone's work).
- `GET /api/wall/stream` drops ids of sessions the caller cannot see; unknown ids still give 404 as
  before. `POST /api/sessions/bulk` reports others' ids as `session not found`, and refuses resume,
  follow-up and checks over budget and accept without `RepoShare.Accept` per id.
- Session `OwnerName` is filled on list, get, create, search and `session` events (not inside the wall
  or feed). Chats, flows and runs get `owner_name` on list and get.
- Host notifications of non-admin owners: sessions and chats call `notify.Notifier.Keep(msg, owner)`
  (an inbox item for that owner only); flow `notify` outputs become owner inbox items. The chat does not
  "say once" that commands are unavailable without bwrap: `run_command` is simply not offered.
- Chat `Access` is asked at every run (`api.ChatAccess`), so a role change applies to the next message;
  `IsAdmin` is a cheap form used under the chat manager's lock. Continued Atrium / Claude chats keep owner
  "" (the legacy owner); those routes are admin only.
- Flow `output` to a file stays in the run's folder (`DATA/flows/runs/<run>/`), which only the run's
  owner and admins can read through the API, rather than `DATA/users/<id>/files`.
- `cerberus user passwd` always sets must-change (as the API reset does). Direct-file mode also revokes
  the user's sign-ins and devices. `user budget` and `user share` write `user.budget` / `user.share`
  audit events in direct mode; through the daemon they are `PATCH /api/users/{id}` records.
- `cerberus doctor` (CLI) opens `users.json` read-only for the users checks; `linger` is checked only
  when the service unit is installed.
- Tests: `routes_matrix_test.go` keeps its own copy of every route's class (a changed class fails
  `TestRouteTableClassified`) and the expected roles per class, and adds `TestRouteMatrixRoles` (five
  roles by login cookie and device, the admin token, personal API keys and MCP clients),
  `TestRouteMatrixMustChange` and `TestRouteMatrixAuditUser`. `users_test.go` covers login, lockout,
  revocation cutting a live SSE, ownership, events filtering, confinement, budgets, attribution, 404,
  per-role status and usage, the admin API, bootstrap and chat scoping.

### As built: review fixes (2026-11-03)

- Confined save: a non-admin's "save" goes through `session.Manager.SaveInto(id, root, dir)`, which
  writes only through an `os.Root` of the caller's `DATA/users/<id>/files` (a planted symlink cannot
  redirect it) and accepts that folder although it sits under DATA. `SaveTo` keeps its checks for admins.
- Confined sandbox: `sandbox.Spec.Confined` now puts a tmpfs over the whole home after `--ro-bind / /`;
  bound back are `KeepRO` (a shared repo's git common dir; the agent's executable when under the home,
  added by `Wrap`), `sandbox.extra_ro`, the profile's `SandboxRW`, the workspace and the session folder.
  The network stays as `sandbox.network` says (the gateway is reached over loopback).
- `local_no_auth`: a personal key (`cuk-`) is checked before the keyless loopback rule, and the rule is
  paused (401 `local_no_auth_paused` for a request with no key) while a confined session runs or a chat
  of a non-admin owner is busy. Doctor `local_no_auth` fails once an enabled role-user account exists.
- MCP clients: sessions record `Client = "mcp:<name>#<device id>"` (a re-paired client of the same name
  inherits nothing), and every agent-scope lookup (`owns`, children lists) also requires the caller's
  owner.
- Session guard sidecar: `DATA/sessions/<id>.guard.json` (outside the agent-writable session folder)
  keeps Owner, Confined, Workspace, Dir, Repo, Worktree, Parent and Client; `load` trusts it over
  `meta.json` (a session from before it gets one from `meta.json` once). `session.WithConfine` makes
  every run, checks pass and resume recompute confinement from the owner's current role.
- Sign-in: `Lockout.Begin/End` and `failLimiter.begin/end` reserve an attempt before the hash runs, so
  in-flight guesses count against the limits; the name is checked again before a login is issued.
  Password sign-ins from loopback skip the per-address limiter (local reverse proxies).
- Removed users: their flows do not run (schedule, triggers, session steps refused with a message) until
  an admin saves them, which adopts the flow (`flows.Engine.Adopt`); delete turns their scheduled flows
  off (`flows_paused`). A session whose stored owner was removed has no agent principal (its token does
  not turn into the receiving admin).
- Disable and delete stop the user's running sessions (`sessions_stopped`); agent requests are no longer
  tracked under `user:<id>`, so role changes and password resets leave running sessions alone.
- `GET /api/leaderboard` takes a per-caller filter (`usage.Sources.Keep`): admins see all, viewers every
  session (no chats, no direct traffic), others their own sessions and chats.
- `/v1`: `X-Cerberus-Session` from a person's key, cookie or device is dropped unless it names a
  session, chat or flow run they own.
- Accounts file: `DATA/users.enabled` is written once an account exists; then a missing `users.json` is
  `users.ErrAccountsMissing` at open (the daemon does not start) and keeps the last list while running.
  Without accounts, a device bound to a user or a confined session's agent token does not bind as admin.
- Confined starts with no agent named use the server default only when the user may use it, else the
  built-in agent (or their first allowed profile).
- Missing `/assets/…` files answer the themed 404 page. `GET /api/auth` adds `signed_in` (a password
  sign-in cookie); Login sends such a browser home. The `/login` redirects keep the query (minus a token),
  and `hardGo` keeps `?theme=`.
- UI: the session view fetches an unknown session first and shows Not found on 404; sign out stops every
  live stream before `POST /api/logout`; the delete dialog reads "Moves to you: …" and mentions stopped
  sessions and held flows; the disable dialog says running sessions stop; the navbar account button
  rule is `#carinoNav .icon-btn.acct-btn` (beats the navbar's `.icon-btn` width).

### As built: gate (2026-11-03)

The full gate ran green on the Phase 4a tree:

- `gofmt -l .` is empty. `go vet ./...` is clean for linux, windows and darwin.
- `go test -race -count=1 ./...` passes in all 33 packages: 1168 tests and subtests pass, 1 is
  skipped and none fail.
- `make cross` builds all six platforms.
- `node --test web/test/*.test.mjs` runs 1208 tests: 1206 pass, 2 are skipped and none fail.
  `go test ./web` passes once `build_sw.py` has run.
- The browser check ran on the demo daemon on 7397, with a fresh data dir. It made 140 checks, all
  passing, and took 63 screenshots. It covers:
  - the first-run screen through the admin token, then creating the first admin;
  - the login page and the generic wrong-password text, at 360, 390 and 1280 in both themes;
  - a per-name lockout after five failures ("Too many attempts. Try again in 15 minutes.");
  - adding one user of each role in Settings → Users, and the forced password change at first
    sign-in;
  - what each role sees:
    - user: chat, sessions, flows, models and usage tabs;
    - viewer: sessions and monitor;
    - monitor: monitor only;
    - chat only: chat only;
  - each role answering 403 on `/api/users` and `/api/config`;
  - a user opening an admin's session by URL: the API answers 404 and the page shows Not found;
  - a viewer reading that session read-only, with "Read only" in the footer and no write buttons,
    while POST input and stop answer 403;
  - chat only: no permission-mode picker, 403 on starting a session, and no tool calls in a
    chat's transcript;
  - an unknown hash route showing Not found;
  - `GET /no-such-page` answering 404 with the themed HTML page;
  - `GET /api/nope` answering a JSON 404 when signed in (401 when signed out, as specified).

  No width shows horizontal scroll. The only console and network errors are the 401, 403 and 404
  answers the checks provoke on purpose, and the streams aborted on navigation.
- One fix came out of the gate. In the Users table at 1280, the status pill "Must change password"
  wrapped as an inline box and broke its border over three lines. `users.css` now makes pills in that
  table `inline-block`.

The browser did not exercise the following. Go tests cover them (`users_test.go`, the route
matrix, `confine_test.go`, `jail_test.go`):
- the sessions of a confined non-admin user (bwrap);
- personal `/v1` keys and budgets;
- paired devices and MCP clients per user;
- the `/api/events` filtering.

This gate step deferred nothing in the spec.

## As built: container image (2026-11-03)

- `Containerfile` (repo root): golang:1.26-alpine build stage (CGO_ENABLED=0, version/build as build args),
  runtime on fedora-minimal:44 with git-core, bubblewrap, curl-minimal and basic tools; user `cerberus`
  (uid 1000); one volume `/var/lib/cerberus` holding XDG config/data/state; `CERBERUS_CONTAINER=1`;
  HEALTHCHECK on `/healthz` (kept with `--format docker`). `.containerignore` keeps the context to the Go
  sources, packaging and `web/` without tests.
- `packaging/container/entrypoint.sh`: first start runs `cerberus init --yes` and sets container defaults
  (lmstudio/llamacpp off, `guard.systemd_slice` empty, `local_no_auth` off, `repo_roots` `/repos`,
  `sandbox.default` bwrap). Every start applies `CERBERUS_LISTEN`, `CERBERUS_PUBLIC_URL`,
  `CERBERUS_OLLAMA_URL`. `CERBERUS_ADMIN_USER` plus a password file (podman secret
  `cerberus_admin_password`) creates the first admin only while no account exists. Non-serve commands run
  as given.
- Quadlet: `cerberus.container` (127.0.0.1:7340, private `cerberus.network`, `AutoUpdate=local`),
  `cerberus-ollama.container` (no published port, GPU through CDI). `compose.yaml` mirrors them.
  Host-loopback Ollama is opt-in (`pasta:--map-host-loopback`) because it exposes every host loopback port
  to agents in the container.
- The sandbox works rootless with `label=disable` + `unmask=/proc/*` (measured: without them bwrap cannot
  mount `/proc` or `/dev/pts`); the default seccomp profile stays, no capabilities are added.
- Doctor: with `CERBERUS_CONTAINER` set, `listen` (wildcard only), `systemd`, `service` and `linger` are ok
  with a container note, `tls` warns with a container fix, and `users_tls` warns instead of failing (the
  daemon cannot see the published port). Public addresses are still refused.
- `cerberus user list --json` prints `[]` instead of `null` with no accounts (the entrypoint tests it).
- Make: `make container` (podman by default, `CONTAINER=docker`), `make container-run`.

## As built: saved permissions (2026-11-03)

Why: approvals interrupted too often; "a" (agent) and "Allow for this session" (chat) were forgotten at
the end of a run or chat.

- **internal/permits** holds the rule language (moved from chat: `Rule{Tool,Content,Text}`, `NewRule`,
  `Matches`; chat keeps `type Rule = permits.Rule`), `Suggest`, `Validate` and the `Store`.
- **Suggest(tool, input, cwd)**, narrowest first, only rules Validate accepts:
  - Bash: the exact command, plus a prefix rule `head:*` or `head sub:*` when the command has no shell
    joins (`;`, `&`, `|`, `<`, `>`, a backquote, `$(` or a line break), its head is a plain name and not risky, and (for
    tools with subcommands: git, go, npm, pnpm, yarn, bun, cargo, pip, uv, poetry, docker, podman,
    kubectl, terraform, helm, dotnet, mvn, gradle, rustup) the subcommand is plain and not refused.
  - Risky heads (no prefix rule): rm, rmdir, mv, cp, ln, install, dd, chmod/chown/chgrp, chattr, setfacl,
    truncate, shred, unlink, tee, sed, awk, tar, unzip; sudo/su/doas/pkexec/runuser; kill/pkill/killall,
    shutdown/reboot/poweroff/halt; fdisk, parted, wipefs, mount, mkfs*; curl, wget, ssh, scp, sftp,
    rsync, nc/ncat/netcat, socat, telnet, ftp; every shell and interpreter (bash, sh, zsh, fish, dash,
    python*, node, deno, perl, ruby, php, lua, java, …); eval, exec, env, xargs, find, fd, rg (`--pre`),
    parallel, watch, nohup, setsid, timeout, nice, strace, gdb, chroot, nsenter, unshare, bwrap, source;
    crontab, at, systemctl, service, journalctl, launchctl; iptables, nft, ufw, firewall-cmd, ip; npx,
    bunx, uvx, pipx, dnf, yum, apt, dpkg, rpm, pacman, brew, flatpak, snap, gh; open, xdg-open.
  - Refused subcommands: git push, reset, clean, checkout, rebase, restore, switch, rm, mv, branch, tag,
    stash, config, remote, submodule, worktree, merge, pull, commit, revert, cherry-pick, am, apply, gc,
    prune, reflog, update-ref, filter-branch/-repo, notes, replace, lfs, grep (`-O`); docker/podman/
    kubectl rm, rmi, kill, stop, run, exec, cp, compose, system, volume, network, prune, push, login,
    delete, apply, create, patch, edit, scale, …; npm/pnpm/yarn publish, exec, x, dlx, token, login, …;
    go env, clean; cargo publish, install, …; terraform apply, destroy, state, …; helm install, upgrade,
    …
  - File tools: the folder (`dir:`), as "Allow for this session" works it out. URL tools: `domain:host`.
    Other tools (cerberus_spawn, MCP tools): tool-wide.
- **Validate** refuses: tool-wide Bash; Bash with shell joins; prefix rules with a risky head, a bare
  subcommand tool (`git:*`) or a refused subcommand; exact commands run with sudo/su/doas/pkexec;
  tool-wide Write/Edit/MultiEdit/NotebookEdit; relative paths (a saved rule applies in every folder, so
  only `dir:/abs`, `dir:~/x`, `//abs/**` and `~/x`); `dir:/` and `dir:$HOME` exactly; an empty or
  malformed `domain:`; control characters; more than 1000 characters. Manual adds go through it too.
- **Store**: `DATA/permits.json`, 0600, atomic (`platform.WritePrivate`); rules carry ID, Owner, Tool,
  Content, Text, Created, LastUsed, Uses, Source (chat | session | settings), at most 500 per owner,
  deduplicated. Use counts are written 5 s later (and on shutdown). A file that does not parse is
  reported and never overwritten: the daemon then runs with no saved rules. Owners compare as stored ids,
  except the single owner from before users ("") which resolves to the first admin; a removed user's
  rules are not handed on.
- **Precedence**: policy deny > policy ask_always (Forced; a saved rule never answers it, and none is
  offered) > policy allow > saved rules > the run's/chat's own rules > the approve mode. (Policy allow is
  decided before any card exists, as before.)
- **Chat**: each pending ask carries `always: [{tool, content, text}]` (in the ask event, GET
  /api/chats/{id} and /api/chats/asks). `POST /api/chats/{id}/answer {decision: "always", option}`
  recomputes Suggest on the server, saves for the chat's owner, answers the card (`answered`
  {decision: "always", rule, saved}) and every other open card the rule covers. The chat-only role
  gets 403 (it has no tools). Cards covered by a saved rule answer `by: "rule"` with the rule text
  starting "your saved permission".
- **Agent sessions**: at every launch of the built-in agent the owner's rules are copied to
  `<session dir>/permits/permits.json` (`$CERBERUS_AGENT_PERMITS`), and rewritten for running sessions
  on every store change. Deviation: the copy lives in a `permits/` folder and the folder is in the
  sandbox's read-only protect list, because a read-only bind of the file itself would keep showing the
  old inode after an atomic rewrite. The agent re-reads it when its mtime or size changes; a covered call
  prints "· allowed by your saved permission …" and emits a `permit;<id>` marker, which the daemon counts
  as a use.
- The agent's prompt is now `[y] yes  [a] always <tool> (this run)  [n] no`, with a dim line pointing to
  Always ▾ → Every time. After the `ask` marker it emits `askrules;<base64url JSON>`; the daemon takes it
  only while an `ask` of the same prompt is pending (once), validates each option, drops any a vault
  value would mask, and shows them as `Info.WaitingAlways []{Text, Tool, Content}`, cleared with
  WaitingAsk. `POST /api/sessions/{id}/answer {answer: "save", seq, option}` (same route and class): seq
  guard as for y/a/n (409 when stale), 400 for an option not offered or one Validate refuses; it saves
  for the session's owner (source session), then types `y`; audited "approval always: <rule>".
- **Hardening**: model text, notes and errors reach the agent's terminal without C0/C1 control
  characters (newline and tab kept), and approval paths are sanitized, so a reply cannot forge a marker.
  run_command's children start with setsid (`platform.NewSession`, a no-op on Windows): no controlling
  terminal, so `/dev/tty` fails; they lead their own group, so Terminate still works, and the terminal
  agent kills what their sessions leave behind when its run ends (`reap`), since the session manager's
  kill of the agent's session no longer reaches them. A process of the same uid can still write to the
  PTY through `/proc/<pid>/fd` or `/dev/pts/N`; the `ask`-before-`askrules` rule and Validate on every
  save limit what a forged prompt could offer, and the person still picks the option.
- **API**: `GET /api/permits` (own rules plus the policy lists, read only, and for admins a link to
  Settings → Safety), `POST /api/permits {tool, content}` (Validate's text in the 400, source settings),
  `DELETE /api/permits/{id}` (404 for another owner's). Class WORK (admin, user); writes audited.
- **Web**: approval.js shows Yes / Always ▾ / No; the menu (views/alwaysmenu.js) has "This run: any
  <tool>" (answer a) and "Every time: …" per option. Chat cards get "Always allow ▾" (Alt+F; no second
  press: picking from the menu is the second step). Settings → Permissions (views/permissions.js) lists
  the rules with source, dates and uses, Remove, an Add form, and the policy lists read only. Users (not
  admins) reach it from Account → Saved permissions; Settings shows them only that page.
- Not done: saving from the terminal (by design: saving must come from the UI or API); saved rules for
  the claude CLI's own permission store (Cerberus answers its asks itself, so they apply there too).

## As built: work previews (2026-11-03)

Owner's request: "if a website is being done, allow to see or visit the website. Either for a shared
rendering space or allowing to open it on another window." Before this, a preview existed only with
`RepoConfig.Preview` or `.cerberus.json` `preview.dir`, lived under `/preview/<id>/~<cap>/` (absolute paths
such as `/style.css` broke), had no dev-server support, vanished once a session was accepted or discarded
(unchanged: the worktree is gone then) and had no in-app view.

**Sources** (`internal/session/preview.go`):

- *Static*: the configured `preview.dir`, else the first of the worktree root, `public/`, `dist/`, `build/`,
  `out/`, `site/`, `_site/`, `docs/` holding a regular `index.html`. The config still wins. Deviation: a
  `preview` object with no `dir` (for example only a `command`) now means "detect", where `{}` used to mean the
  root; the root is the first candidate anyway. Served while the worktree exists and the session is not
  accepted or discarded (so also for passed, failed, waiting and stopped sessions). While a session is live,
  the 5 s preview tick refreshes `PreviewURL` (an agent's new `index.html` shows without a state change).
- *Live*: TCP sockets in LISTEN state on loopback or the wildcard, in the daemon's network namespace
  (`/proc/self/net/tcp{,6}`), whose inode is open in a process the session owns (`/proc/<pid>/fd`). Owned
  means: the agent's PTY session (sid) and every descendant; the run's systemd scope (cgroup) when there is
  one; the Run preview server's session and descendants; any process whose environment carries
  `CERBERUS_PREVIEW_TAG=<HMAC of the session id with a per-daemon random key>`. The tag is set in the agent's
  environment and the built-in agent's `run_command` keeps it (it strips the other `CERBERUS_*`), so a dev
  server that daemonised out of the tree still counts; it carries no authority, and another session cannot
  compute it. One `/proc` pass serves every session (`procIndex`); results are cached 1 s. Other OSes:
  `platform.SocketScan` is false, nothing is detected, and only the Run preview server counts, on the port
  Cerberus gave it. `Info.Preview` = `{Static, Live: [ports], Managed: state}` (filled on read, not stored);
  the 5 s tick rescans live sessions, sessions with a managed server and sessions that had live ports, and
  emits a session event when the ports change.
- *Run preview* (managed): the command from `RepoConfig.Preview.Command` or `.cerberus.json`
  `preview.command`, else `package.json` scripts `dev`, `start`, `preview` (first present) run with the
  lockfile's tool (pnpm, yarn, bun, else npm); scripts that mention vite, astro or next get their port and
  host flags (`--port {port} --strictPort --host 127.0.0.1`, `--port {port} --host 127.0.0.1`,
  `-p {port} -H 127.0.0.1`; npm adds `--`). `{port}` and `PORT` are a free loopback port Cerberus picked;
  `BROWSER=none`; the environment drops `CERBERUS_*` and `OPENAI_API_KEY`. It runs `bash -c` (Windows: `cmd /C`)
  in the worktree, inside the session's sandbox plan (`planSandbox`: always bwrap for a confined owner; refused
  when that sandbox has no network, since the server could not be reached) and the session's memory scope
  (`wrapScope`), in its own session (setsid). Output goes to a 16 KiB tail. States: starting → running (once
  an owned socket listens; elsewhere once the port answers) → exited | failed | stopped. It stops (SIGTERM to
  its session, SIGKILL after 3 s) on Stop preview, discard, accept (before the worktree goes), delete, prune,
  daemon shutdown, and after `sessions.preview_idle_min` (default 30) without preview requests.

**Origins** (`internal/api/previewsrv.go`, `server.preview_ports`, e.g. `7350-7369`, at most 200 ports, empty =
off, validated clear of the listen ports):

- A *lease* is one preview (a session's static site, or one live port) on one port of the range. Ports are
  bound lazily, on the entry request, on every distinct host of `listen` and `extra_listen` (HTTPS with the
  app's certificate on extra addresses when TLS is set; a host bound both ways takes `listen`'s scheme), with
  the same bind safety as the app (public addresses refused unless `allow_public`). A busy port is skipped.
  Leases end when the session is accepted, discarded or removed (session events), when the source goes away
  (no static dir, the port no longer owned), after `preview_idle_min` without requests (15 s sweep), on Stop
  preview (its port), and at shutdown.
- Entry: `GET /preview/<id>/[path][?src=<port>]` on the app (class WATCH, owner scoping as for sessions:
  others get 404) leases the origin and redirects to `<scheme>://<host the browser used>:<port>/<path>
  ?cerberus_preview=<cap>`. Without preview ports the static entry keeps the old capability path and
  `?src=` answers 404. The API and UI hand out only entry URLs, so a copied link needs a sign-in.
- Auth on the origin never uses the app's credentials. `cap` = first 40 hex of HMAC-SHA256(admin token,
  "cerberus preview origin" ‖ session ‖ source ‖ port ‖ a random per-lease nonce). The origin checks it,
  sets `cerberus_pv_<port>` (HttpOnly, SameSite=Lax, Secure on TLS) and redirects without it; every other
  request needs that cookie. Cookies are per host, not per port, hence one name per port, and a new nonce
  per lease, so a port reused by another session never honours an older cookie. The Host header must be ours.
  The first entry of a lease answers `Clear-Site-Data: "cache", "storage"` (not "cookies": they are the
  host's, the app's included). Requests with a `Service-Worker` header are refused: a worker would outlive the
  lease and answer for the next preview on that port.
- CSP decision: on a preview origin scripts and same-origin access are allowed (no `sandbox`), because the
  page is alone on its origin: it cannot touch the app's DOM or storage, the app refuses its API calls, and
  real sites need localStorage, cookies of their own, `fetch` to their own origin and WebSockets. Only
  `frame-ancestors <the app's origins>` is set (added to a dev server's own CSP, which the browser
  intersects). The Preview tab's iframe adds `sandbox="allow-scripts allow-same-origin allow-forms
  allow-popups allow-modals allow-downloads"`: no top navigation. The path mode keeps `sandbox` as before.
  The app's own CSP gains `frame-src 'self' http://<host>:* https://<host>:*` when preview ports are on.
- The app refuses a preview page: cookie-authenticated `/api` requests with `Sec-Fetch-Site` other than
  same-origin/none (a preview port is same-site), with `Origin: null`, or with an `Origin` on one of this
  host's preview ports (`fromPreviewOrigin`, for browsers without fetch metadata) get 403; writes still need
  `X-Cerberus: 1`, which needs a preflight that is never approved (no CORS headers at all). Two different
  `cerberus_token` cookies on one request are refused (401) instead of picking one: a preview can plant a
  cookie with a narrower path that the browser sends first.
- Proxy (live sources): `httputil.ReverseProxy` to `127.0.0.1:<port>` (`[::1]` for a v6-only loopback
  socket), WebSocket upgrades included (HMR). `Host` and `Origin` become `localhost:<port>` (Vite and others
  check them), `Referer` is rewritten to match; Cerberus' cookies, `X-Cerberus*`, `X-Forwarded-For`,
  `Forwarded` and a bearer that Cerberus would accept are removed; responses lose any `Set-Cookie` of
  `cerberus_token` or `cerberus_pv_*`, and an absolute `Location` on `localhost:<port>` becomes relative.
  Ownership is re-checked at most once a second; a port the session no longer owns answers 502.

**API** (route table above): `GET /api/sessions/{id}/preview` → `{ports, detect, static: {url, dir} | null,
live: [{port, url, source: "detected"|"managed", state}], managed: {state, command, source, port, error,
started, log_tail}, can_start}` (`url` values are app entry paths; live `url` is "" without preview ports).
`POST …/preview/start` (400 `no_command`, 503 `sandbox`) and `POST …/preview/stop` answer the same object and
are audited ("preview server: <command>"). Viewers get `can_start: false` and 403 on the posts.

**Web**: `views/preview.js` is the session's **Preview** tab (`#/s/<id>/preview`, between Files and Replay):
an iframe of the chosen source with a source picker (Static (dir) / Live :port / Run preview :port; the
choice sticks while it exists, else managed, then live, then static), width presets Phone 390, Tablet 768,
Desktop (a preset wider than the panel fills it), Reload (re-enters, so a closed origin reopens), **Open in
new window** (`window.open(url, '_blank', 'noopener')`), Copy link (the absolute entry URL), the Run preview
row (state pill, command with its source, Start/Stop for the owner and admins, log) and an empty state that
says how a preview appears (with the detected command). It fetches on open, on the session's `Preview`
summary changing, and every 4 s while shown and visible. Phones: the iframe fills the width, presets are
hidden, Open in new window spans the bar, and the six session tabs scroll in their own row. Session rows and
wall tiles get a "Preview ↗" chip (`previewChip`: first live port, else the static site) that opens a new
window; the session's action bar keeps a "Preview ↗" link. Pure helpers are in `assets/js/preview.js`
(`web/test/preview.test.mjs`). Styles: `assets/preview.css`.

**Container**: the Quadlet unit, `compose.yaml` and `make container-run` publish `127.0.0.1:7350-7369`; the
entrypoint's first start sets `server.preview_ports` to `7350-7369`. **Doctor**: check `preview_ports`
(off is ok; fail on a bad range, an overlap with a listen port or a public host without `allow_public`; warn
when no port of the range is free or on wildcard hosts outside a container; it reports how many are free).

**Tests**: `internal/session/preview_test.go` (static detection order, config wins, package.json commands),
`preview_linux_test.go` (Run preview lifecycle with a real server: running, log, owned port, idle stop,
discard; Stop; no command; live ownership of a child server and of a daemonised one through the tag, and a
listener of another process never owned), `internal/api/previewsrv_test.go` (origin serving with absolute
paths, wrong capability, no cookie, the app's cookie and token refused on the origin, foreign Host, service
worker refused, another session's cookie or capability, old cookie after a new lease, Clear-Site-Data, the
preview cookie refused by the app, conflicting cookies, a preview page's GET/POST refused by Origin and
fetch metadata, no CORS on preflight, the app CSP's frame-src, proxy Host/Origin/cookie handling both ways,
Location rewrite, a WebSocket round trip, unowned port 404 and 502, sweep, the API, path mode),
`doctor_test.go` (`TestPreviewPortsCheck`), the route matrix rows, and the web helpers.

**Not done / limits**: previews behind a reverse proxy that forwards only the app's port (`tailscale serve`,
Caddy) do not reach the preview ports; the URL uses the host the browser used and the app request's scheme.
Live detection is Linux only. Cookies set by one preview page are visible to other previews on the same host
(cookies are not port-scoped); previews are not isolated from each other beyond their origins. The Run
preview server is not counted by the session guard (`max_sessions`, free memory); its memory cap is the
session's scope when systemd scopes are available.

---

# Gateway phase specification (G1 to G6)

Ideas taken from halogen-flash-server (github.com/peonist-ai/halogen-flash-server, read 2026-11-05).
That project ships as a closed container image under an EULA and runs on one AMD GPU, so **nothing is
copied**: no code, no prompt files, no model files from it. Only the serving design carries over, rebuilt
on what Cerberus already supervises (llama.cpp, LM Studio, Ollama, cloud providers). "Never write an
inference engine" still holds: every item below lives in the gateway, the engine manager or the CLI.

The general rules still apply. Config is add-only and every new key carries schema metadata. Every new
route goes in the route table and the route matrix. State changes are audited. Everything is off or
cheap when idle: no item adds polling while no request is in flight.

Order: G4 (small, makes G1 cleaner) → G1 → G3 → G5 → G2 → G6.

## G1. Messages and Responses APIs (`internal/gateway`)

Today the gateway serves `/chat/completions`, `/completions` and `/embeddings` only. Add:

- `POST /v1/messages` and `POST /v1/messages/count_tokens` (Anthropic Messages API), and
- `POST /v1/responses` (OpenAI Responses API),

so that Claude Code, the `anthropic` SDK and Codex CLI can drive a **local** model, or an API-key
provider, through Cerberus.

**Routing.**
- A model on the `anthropic` provider gets `/v1/messages` passed through natively to the provider's own
  Messages endpoint, with the provider's key from the vault (it is an API key, so this is not subscription
  proxying).
- An OpenAI-style provider that declares `responses: true` in its `ProviderConfig` gets `/v1/responses`
  passed through.
- Everything else is **translated** to `/chat/completions` on the way in and back on the way out, streamed.
  Translation lives in `internal/gateway/xlate/` (pure, table-tested): `messages.go`, `responses.go`.

**What the Messages translation covers.**
- `system` (a string or text blocks), and `system` turns inside `messages`, which become user text.
- `text`, `image` (base64 and URL, only when the model has `Vision`), `tool_use`, `tool_result` (with
  `is_error`) and `thinking` blocks.
- `tools` and `tool_choice` (`auto`, `any` becomes `required`, `tool`, `none`); `stop_sequences`;
  `temperature`, `top_p` and `top_k`; `max_tokens`.
- `thinking` (`enabled` with `budget_tokens`, `adaptive`, `disabled`) and `output_config.effort` become
  `reasoning_effort` plus `chat_template_kwargs.enable_thinking`; `output_config.format` (a JSON schema)
  becomes `response_format`.
- **Thinking round trip.** A returned `thinking` block's `signature` is the base64 of the model's reasoning
  text. When a client sends the block back, its text is restored, so the next prompt repeats the last one
  byte for byte and the backend's prompt cache resumes it.
- **Streaming.** SSE events in Anthropic's order: `message_start`, then `content_block_start`, `delta` and
  `stop` per block (thinking, text, tool_use with `input_json_delta`), then `message_delta` with
  `stop_reason` and usage, then `message_stop`. Keepalive `ping` events go out every 15 s while upstream
  is quiet.
- **Errors** come back in the Messages error shape (`{"type":"error","error":{"type","message"}}`), with
  the real HTTP status.
- **`count_tokens`** uses the backend's `/tokenize` (llama.cpp) over the rendered prompt when it exists,
  else an estimate of 4 bytes per token, marked `"estimated": true`.
- **Dropped, each named once in the log:**
  - Anthropic server-side tools (web search, code execution); the client's own tools are used;
  - `cache_control` and `context_management`.
- **Refused with a 400:** PDF document blocks, and the Batches and Files APIs (404).

**What the Responses translation covers.**
- Input items, `instructions`, `function_call` and `function_call_output` items, `reasoning` items (with
  their text kept for the round trip), and `text.format` schemas.
- There is **no response store**: a `previous_response_id` that comes without the full history is a 400
  that says so. Cancellation is by disconnect.

**Model names.** The `model` field resolves like any other gateway model. An unknown `claude-*` or `gpt-*`
name falls back to the profile's or the config's mapped local model (`gateway.model_map`, for example
`{"claude-*": "default"}`), because clients hard-code their vendor's names. Replies name the model that
actually served them.

**Agent profiles** (new defaults; config overrides by name as usual):
- `claude-local`: `claude {task}` with `ANTHROPIC_BASE_URL={gateway_root}`,
  `ANTHROPIC_AUTH_TOKEN={session_token}`, `ANTHROPIC_MODEL={model}`,
  `ANTHROPIC_SMALL_FAST_MODEL={model}` and `CLAUDE_CODE_MAX_OUTPUT_TOKENS` set to the model's output cap.
  `NeedsModel: true`. Its description says "Claude Code against a local or API model through the gateway
  (no subscription)".
- `codex`: `codex exec {task}` with a generated `CODEX_HOME/config.toml` that sets `model_provider =
  "cerberus"`, `base_url = "{base_url}"` and `wire_api = "responses"`. `NeedsModel: true`.

Both work with no Anthropic or OpenAI account. Vendor-neutral holds, because Claude Code is still only an
optional profile.

**Usage.** `usage.Record` gains `API string` (`chat`, `completions`, `messages`, `responses`,
`embeddings`). The usage page groups by it.

**Tests.** Table tests for both translators: every block kind both ways; a streamed tool-use reply,
event by event; a thinking round trip that is byte-identical; error shapes. An end-to-end gateway test
against the fake engine. A golden transcript of what Claude Code sends (captured once, kept in testdata
with no secrets).

## G2. Agent guard (`internal/guard`, new)

An agent reads tool results: files, web pages, issues, emails. Any of them can carry text aimed at the
agent. `docs/SECURITY.md` lists prompt injection as open. The guard is a second reader for that case. It is
**not a security boundary**, and the UI and docs say so wherever it appears.

**Config** (`gateway.guard`):

| key | meaning |
|---|---|
| `mode` | `off` (default), `watch` (score and report, change nothing) or `hold` (as `watch`, and in a turn with a flagged result, hold tool calls that act outside the conversation) |
| `model` | a local classifier model id. The suggested one is Qwen3Guard-Gen-0.6B as a GGUF (Apache-2.0), and `cerberus doctor` names it when `mode` is on and the model is missing. It goes through `engine.Manager` like any model: loaded on first use, reaped when idle |
| `threshold` | flag score, default 0.5 |
| `wait_ms` | longest a tool call waits for pending verdicts, default 10000; after that it goes out unscreened and is logged |
| `window_tokens` | long results are read in overlapping windows of this size (default 512) and scored by the largest window |
| `side_effect_tools` | tool-name globs that act outside the conversation (`bash`, `*write*`, `*edit*`, `*send*`, `*post*`, `*delete*`, `mcp__*__create*`, ...). Defaults ship in code; config adds. A name that matches nothing is treated as side-effecting under `hold` |

**What it does.**
- On a request that carries tool results (`tool` messages, `tool_result` blocks, `function_call_output`
  items), each result **new since this conversation's last request** is scored. The conversation is keyed
  by session id plus a hash of the message prefix. Scoring runs in parallel with the main model's prefill,
  so a clean turn streams with no added wait.
- **A score** is one forward pass on the classifier: a fixed prompt that ends where the label goes,
  `max_tokens: 1`, `logprobs` and `top_logprobs`, read as P(unsafe). This is the same logprob-classifier
  pattern a Flows classify step can reuse later.
- **In `hold` mode**, a flagged turn ends differently. The gateway buffers the reply's tool calls. For each
  call that matches `side_effect_tools` and was not asked for in the user's own messages (a plain
  substring check on the tool name and its main argument), the reply is replaced by an ordinary assistant
  message, in the API's own shape:
  - what was flagged (which tool result, its score);
  - which call was not made, and why;
  - that nothing was sent or changed, and that the user can say "go ahead".

  A call that reads only, and a forced call (`tool_choice` naming a tool), are reported, not held.
- The user's next message starts a new turn, and the guard carries no flag into it.
- **Under Cerberus sessions**, a held call also posts to the session's activity feed and the inbox with
  "Approve once": it resends the turn with a marker header the guard honours for that one call. It is
  audited.

**Reporting.**
- `/v1/chat/completions` replies carry `cerberus_guard` (mode, threshold, one entry per tool result
  with its score, tainted, calls checked and held). A streamed chat completion carries it in one chunk
  without choices, ahead of `[DONE]`.
- `usage.Record` gains `GuardFlagged int` and `GuardHeld int`.
- `/api/status` gains `guard: {mode, model, screened, flagged, held, errors}` (counters since start).
- The Monitor page shows the counters; a session's feed shows each hold.

**Limits** (copied into `docs/SECURITY.md`):
- an injection it does not flag passes;
- an attack that needs no side-effecting call has nothing to hold;
- coding-agent recall is unmeasured;
- benign READMEs that instruct can be flagged;
- long results are where it misses most.

**Tests.** Use a fake classifier engine that scores by a marker string. Cover: `watch` changes no bytes of
a reply; `hold` replaces a side-effecting call and keeps a read-only one; a forced call is reported; the
`wait_ms` timeout lets the call out; approve-once; all three APIs.

## G3. Request records (`internal/records`, new)

`usage.Record` holds counts and timings, and the exchange log (`internal/gateway/exchange.go`) keeps
cloud chat only, for fine-tuning. Request records answer a different question: **what do my agents and
clients actually send, and what comes back?** Off by default.

**Config** (`gateway.records`):

| key | meaning |
|---|---|
| `enabled` | default false |
| `text` | `none` (default), `user` (user and assistant text only) or `all` (tool results and system prompts too) |
| `dir` | default `DATA/records` |
| `retain_days` | default 14 |
| `max_mib` | default 1024; the oldest days go first |

**Files.** `turns/<YYYY-MM-DD>/<pid>-<ms>.jsonl` holds one line per request, appended when it finishes,
with a new file every day (UTC) and every 64 MiB. The files are mode 0600. A line carries:

- **who sent it:** the API (`chat`, `messages`, `responses`, `completions`), streamed or not, the model and
  backend, the session id, the user (Phase 4a), and the client family read from `User-Agent`
  (`claude-code`, `codex`, `opencode`, `aider`, `cerberus-agent`, `openai-sdk`, `anthropic-sdk`, `curl`,
  `other`);
- **what it asked for:** the message count, the tool count, the image count, prompt tokens and cached
  tokens (from the backend's usage or `timings`), thinking on or off, the effort, sampled or greedy, and
  whether a schema was set;
- **how it ended:** stop, length, length in thinking (G4), tool calls, disconnect, error or held (G2);
  completion tokens and the reasoning tokens among them; TTFT and duration;
- **the tools:** the name and id of each tool call in the reply, and, for each tool result sent back,
  whether it answers one of the previous calls and whether the client marked it an error;
- **the text:** only at the configured `text` level, clipped to 256 KiB per field, and with the
  vault's redaction applied (known secrets and token shapes).

**API** (admin only, audited reads of text):
- `GET /api/records?day=&session=&client=&limit=` lists lines without text.
- `GET /api/records/{day}/{offset}` returns one line with text.
- `DELETE /api/records?before=` deletes records.

**Web.** A Records tab under Usage: a table filtered by client and session, with shape charts (prompt size
over a session, cache-hit ratio, tool-error rate per tool). A row opens the full line.

**Doctor.** Warn when `text` is not `none` while the gateway listens beyond loopback. Users and roles
(Phase 4a) note: the text is personal data under LFPDPPP, so the Users page shows whether records are on.

**Tests.** Rotation by day and by size; retention; `text=none` writes no text field at all; redaction;
the client family table; reads of text are audited; non-admins get 403.

## G4. Token budgets (`internal/gateway`)

- **One budget field with three names.** `max_tokens`, `max_completion_tokens` and `max_output_tokens`
  are read as one. Several that agree are fine. Two different values are a 400 that names both. The
  upstream gets the name its backend expects (`max_tokens` for llama.cpp, LM Studio and Ollama, the
  provider's own for cloud).
- **Per-model defaults.** `ModelConfig.Defaults` (new, add-only): `{reasoning_effort, enable_thinking,
  max_tokens, max_tokens_cap, temperature, top_p, top_k}`. They are applied only when the request names
  none of the corresponding fields. Agent harnesses send no thinking control to a custom endpoint, so this
  is what they run at. `max_tokens_cap` turns a larger request into the cap, logged once per client, the
  same way Claude Code's maximum output request is clipped.
- **Length in thinking.** The gateway already counts content deltas. When a reply ends with
  `finish_reason: length` and produced reasoning but no content, the record says `length_in_thinking`.
  The Chat view and the built-in agent show "the token budget ran out while the model was thinking:
  raise the budget or lower the effort" instead of an empty answer. The reply itself is not changed.
- `GET /v1/models` entries gain `cerberus: {ctx, max_tokens_default, max_tokens_cap, vision,
  reasoning_efforts}` so clients and the web can read the limits.

**Tests.** The alias matrix (agree, disagree, absent); defaults applied and overridden; cap clipping;
length-in-thinking from a fake stream.

## G5. Stall watchdog and honest status (`internal/engine`, `internal/gateway`)

A connect is not a health check: a local engine whose GPU queue has hung still accepts TCP, and llama.cpp
can answer `/health` from its HTTP thread while every slot is stuck.

- **Only while requests are in flight**, for each local model: if a request has had no upstream byte for
  `Engines.StallSec` (new, default 120; a running prefill counts as progress where the backend reports it,
  as llama.cpp's `/slots` does), the manager probes `GET /slots` (llama.cpp) or `/api/ps` (Ollama), or the
  models list (LM Studio), with a 5 s timeout. If the probe also fails or shows no progress, the model is
  **stalled**:
  - its waiters fail with 503 `engine_stalled`;
  - the in-flight requests are cancelled;
  - the existing crash and reload path restarts it (llama.cpp) or unloads and reloads it (others);
  - a notification goes out, and the event is audited.

  Nothing polls while the model is idle.
- `/api/status` models gain `queued`, `longest_wait_s` (the oldest waiter in the model queue), `busy_for_s`
  (the oldest in-flight request) and `stalls` (a count since start). The Monitor and Models views show
  them, and a model that has been busy for more than a minute shows a "busy for N s" line.
- **Long prompts.** For llama.cpp, streamed requests ask for `return_progress` where the build supports
  it (probed once per binary). `prompt_progress` frames become a "reading prompt P/T" line in the Chat
  view and in the session's model line. Clients that did not ask for them never see the frames: the
  gateway strips them.

**Tests.** Use a fake engine that stops writing mid-stream and fails `/slots`. Check that waiters get 503,
the model reloads and one notification goes out. Check that a slow engine still showing progress is not
stalled, and that idle means zero probes (a counter in the test).

## G6. Stable bench (`cmd/cerberus bench`, `internal/bench`, new)

The ground for the Phase 3 tokens-per-joule tuner and for the Leaderboard.

- **The prompt set.** `internal/bench/prompts.json` is embedded and written fresh for Cerberus, not
  copied. It holds six kinds of prompt, two prompts each: code, edit (a whole-file rewrite), proof,
  procedure, chat and prose (es and en). It also holds three prefill sizes: 8k, 32k and the model's ctx/2,
  built from fixed filler. **The file is append-only.** A changed prompt gets a new name, so runs across
  versions stay comparable, and a test fails if an existing entry's hash changes.
- **The run.** `cerberus bench [--model ID] [--repeat 3] [--kinds code,prose] [--prefill]
  [--label TEXT] [--json]` drives the gateway with temperature 0 and thinking off, unless the flags say
  otherwise. Per kind it records:
  - decode tok/s;
  - TTFT;
  - prefill tok/s;
  - energy from `internal/energy` (J, tok/J), with the medians and the spread.

  It refuses to run while agent sessions are running, unless `--force` is given, and it says why.
- **The results** go to `DATA/bench/<time>-<model>.json`, carrying the model's config (backend args, ctx,
  parallel, draft model, power limit, the Cerberus version and llama.cpp build).
- **Comparing runs.** `cerberus bench compare A B` prints a table per kind: A, B, the change and a
  plain-language line, such as "decode +6% on code, prefill flat".
- **Speculative decoding.** `ModelConfig.Draft` already exists. The bench reports, per kind, how much a
  configured draft model gains, which is the evidence the Phase 3 speculative presets need.
- **API and web.**
  - `GET /api/bench` lists runs; `POST /api/bench` (admin, audited) starts a run as a background job;
    `GET /api/bench/{id}` returns one.
  - The Leaderboard gains a "Bench" metric: tok/s and tok/J per model at the same label.
- **Tests** run against the fake engine only. The fleet rule holds: no real model is ever loaded onto the
  GPU in tests.

## Not taken

These are engine internals for one GPU:
- custom kernels;
- the model's own draft head and prompt lookup speculation;
- the NPU sidecar;
- the KV pool layout, 1M context and the attention budget;
- the two-container split;
- the pause-on-disconnect retry grace;
- `max_thinking_tokens` (closing a think block mid-stream needs the engine).

**Keep and fork** of KV state for Flows fan-out (llama.cpp `--slot-save-path`) is a Phase 3 candidate,
not part of this phase.

## Acceptance (gateway phase)

- Claude Code (`claude-local`) and Codex (`codex`) each finish a small edit task in a worktree against
  a local model, through the gateway, with no vendor account present.
- With `guard.mode=hold`, a tool result carrying a planted instruction makes the next `bash` call come
  back as the held message, and "go ahead" continues.
- Records on with `text=none`: a session's requests appear in Records with client family, sizes, tools
  and finish reasons, and no text anywhere on disk.
- Two budget fields that disagree are a 400. A model's `Defaults` apply to a harness that sends nothing.
  An empty reply that ran out in thinking says so in the Chat view.
- A fake engine that hangs is detected within `StallSec + 5 s`, its waiters get 503 and it is reloaded.
  Idle means zero probes.
- `cerberus bench` and `bench compare` run end to end against the fake engine. `prompts.json` hashes are
  pinned.
- The gate in CLAUDE.md passes, and the route matrix classifies every new route.

## Gateway phase: as built (2026-11-05)

Built in the order G4, G1, G3, G5, G2, G6. The gate in CLAUDE.md passes (vet on linux/windows/darwin, race
tests of every package, `make cross`, node tests, `go test ./web`). Deviations from the specification above:

**G4 token budgets** ([internal/gateway/budget.go](../internal/gateway/budget.go)). As specified. The budget check
runs before the model is acquired, so a bad budget never loads a model. `/v1/models` entries carry
`cerberus: {ctx, vision, max_tokens_default, max_tokens_cap, reasoning_effort_default, reasoning_efforts,
token_budget_aliases}`. The built-in agent (and so the Chat view) shows `ThinkingBudgetNote` as a note event.

**G1 Messages and Responses APIs** ([internal/gateway/xlate/](../internal/gateway/xlate),
[internal/gateway/apis.go](../internal/gateway/apis.go)).
- A thinking `signature` (and Responses `encrypted_content`) is `cerberus1:` + base64 of the text, so a real
  Anthropic signature is never decoded.
- `/messages/count_tokens` uses llama-server's `/apply-template` + `/tokenize` only for a llama.cpp model that is
  already loaded (counting never loads a model). A native Messages provider gets it passed through; anything else
  is an estimate marked `"estimated": true`.
- Native pass-through (`providers[].apis`) moves the key to `x-api-key` and sets `anthropic-version` for the
  Anthropic API. Usage of native replies is read from their own events (`message_start`, `message_delta`,
  `response.*`).
- The API layer accepts the token as `x-api-key` on `/v1` only.
- New placeholders: `{server_url}` (the root without `/v1`) and `{api_key}`, which works in a profile's `env`
  only, never in a command line.
- The `codex` profile configures its provider with `-c` overrides, not a generated `CODEX_HOME`.
- Translated streams keep the usage chunk (`request.keepUsage`). The keepalive is a `ping` event (Messages) or
  an SSE comment (Responses) after 15 s of quiet.
- Verified with the real Claude Code CLI (`claude -p` against a fake engine: a hard-coded `claude-sonnet-4-5`
  mapped through `gateway.model_map`, streamed and printed). Codex CLI was not installed, so the Responses route
  is covered by tests only.

**G3 request records** ([internal/records/](../internal/records),
[internal/gateway/records.go](../internal/gateway/records.go), [internal/api/records.go](../internal/api/records.go)).
- A line's address is `GET /api/records/{day}/{file}/{offset}`, because one day can hold several files.
- The Messages API marks tool errors per result, but a line keeps counts (`results`, `result_errors`,
  `matched`), so the web shows tool errors per client, not per tool.
- `/api/status` carries `records` (on, text level, written, dropped) for admins.
- The web section is admin-only (the `audit` capability).

**G5 stall watchdog** ([internal/gateway/stall.go](../internal/gateway/stall.go)).
- An engine counts as stalled after two failed probes in a row, past `stall_sec` of quiet.
- A `/slots` answer of 404 or 501 (llama-server `--no-slots`) counts as healthy, since it gives no signal.
- A stall sends a notification and a log line, not an audit record (the gateway has no audit log).
- `prompt_progress` is requested on every streamed llama.cpp request without probing the binary (llama-server
  ignores unknown fields). It is stripped for clients that did not ask, and it counts as output for the
  watchdog.
- **Not done:** the "reading prompt P/T" line in the Chat view and the session's model line.
- Busy, queue and stall figures show in the Models view only (`busyText`), not in the Monitor.

**G2 agent guard** ([internal/gateway/guard.go](../internal/gateway/guard.go),
[guard_relay.go](../internal/gateway/guard_relay.go)).
- The config type is `config.AgentGuardConfig`, because `GuardConfig` is the session guard. It gains
  `read_only_tools` beside `side_effect_tools`.
- A tool's class, in order:
  1. a configured read-only pattern;
  2. a configured side-effect pattern;
  3. a named built-in reader (`TodoWrite`, `update_plan`);
  4. a built-in side-effect pattern;
  5. a built-in read-only pattern;
  6. otherwise it acts.
- "New" tool results are those after the last assistant message, so the guard keeps no state per
  conversation.
- The classifier reply is read in Qwen3Guard-Gen's format. A window counts only when its label is Unsafe or
  Controversial and the categories name Jailbreak. Violent or other unsafe content is not treated as an
  injection.
- `watch` never delays a reply: the `cerberus_guard` chunk carries the verdicts if they are in. The usage and
  records lines wait for them (`finish`).
- **Not done:** "Approve once" in the inbox and the feed entry. A held call notifies with `Kind: "guard"`, and
  the user answers "go ahead", which is a new turn with nothing to screen.
- Counters: `/api/status` → `agent_guard` (admins), the line at the top of Usage → Request records, the usage
  record (`GuardFlagged`, `GuardHeld`) and the records line (`guard`). Doctor gained `agent_guard`.
- Tested with a fake classifier only. The suggested model is in `~/models/Qwen3Guard-Gen-0.6B.Q8_0.gguf`
  (mradermacher's Q8_0 GGUF, Apache-2.0, SHA-256 127e8833…411a), but it has not been run: no llama-server was on
  PATH, and tests never load models.

**G6 bench** ([internal/bench/](../internal/bench), [cmd/cerberus/bench.go](../cmd/cerberus/bench.go),
[internal/api/bench.go](../internal/api/bench.go)).
- Energy per request comes from the usage records, matched by the run's `X-Cerberus-Session` tag.
- Each run starts with one unmeasured warm-up request, which loads the model.
- `prompts.json` is pinned per entry by hash in `bench_test.go`.
- There is no llama.cpp build string in the run's config (the gateway cannot reach the engine's `/props`).
- The web is a "Bench" section in Usage (runs plus a start form for admins), not a Leaderboard metric.
- `cerberus bench list` and `bench compare A B` are as specified. Compare names a draft model when only one side
  has one: that is the speculative-decoding gain.

**Browser check:** not run yet. The three test ports (7381, 7397, 7398) were held by other sessions' static
servers and the demo harness folder was gone. The new web pieces are:
- Usage → Request records;
- Usage → Bench;
- the Models load line.

Their helpers have node tests.
