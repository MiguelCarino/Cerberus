# Cerberus

Cerberus is Carino Systems' self-hosted LLM runtime. It is one small Go service with three jobs,
which give it its name:

1. **Models.** It runs local LLMs through llama.cpp (`llama-server`), LM Studio (`lms`) or Ollama.
   It loads a model when a request needs it, unloads it when it goes idle, and makes room in VRAM
   first. Every request is recorded with its time to first token, tokens per second and energy.
2. **Gateway.** It serves one OpenAI-compatible endpoint (`/v1`). Each request goes to a local model
   or to a cloud provider that you reach with your own API key.
3. **Agents.** It runs coding-agent sessions: its own built-in agent or any CLI agent you configure.
   Leave them running and steer them from a web UI on your phone or desktop: watch the terminal,
   answer approvals, review the diff, then accept or discard.

Around those three parts it has a control center for many agents, flows (saved, scheduled recipes
of model calls and agent sessions), a file drop, safety controls, an LLM chat, and a few extras (an inbox, an installable app, session replay, a model downloader and a
leaderboard).

**Vendor-neutral.** Everything works with no Anthropic or Claude software installed. Claude Code is
one optional agent profile and one optional chat backend; Anthropic is one optional provider.
**Subscriptions are never proxied.** A consumer subscription is used only by running its official CLI
as a session. API keys go through the gateway.

Cerberus is the service half of a service plus desktop pair. Today the desktop half is Atrium's LLM
chat, which uses Cerberus as an ordinary OpenAI-compatible server (see [docs/DESKTOP.md](docs/DESKTOP.md)).

**Status:** pre-release. Linux first. The code is pure Go (`CGO_ENABLED=0`) and also builds for
macOS and Windows. Agent sessions need a PTY, which Windows does not have yet. `install-service` is
Linux only.

## Install

You need Go 1.26 or later to build, and `git` and `bash` for sessions. Optional: `llama-server`, LM
Studio, Ollama; `nvidia-smi` (NVIDIA) or the amdgpu driver (AMD) for GPU readings; `bwrap` for the
sandbox; `ffmpeg` and a local Whisper for voice; Tailscale for phone access.

```sh
make build                        # dist/cerberus: one static binary, version 0.0.<commits>
./dist/cerberus init              # looks at the machine, asks at most 6 questions
./dist/cerberus install-service   # copies to ~/.local/bin, installs and starts the systemd user unit
./dist/cerberus url               # sign-in link for each listen address
```

- `cerberus init` finds engines and models, the GPU, agent CLIs, Tailscale, the notify script, voice,
  bwrap and RAPL. It asks about the default model, phone access, an HTTPS certificate, an ntfy topic,
  your household kWh and the service, then writes the config and runs the doctor. `--yes` takes the
  defaults and keeps the daemon on loopback.
- Instead of the service you can run `cerberus serve` by hand. Without a config, the first run
  writes the defaults with a fresh token.
- The daemon listens on `127.0.0.1:7340`. Open the link from `cerberus url`: it signs the browser in
  and drops the token from the address bar.
- A config made without `init` gets a short welcome in the web UI (model, phone access,
  notifications, electricity). You can skip it.
- `cerberus doctor` checks the setup at any time and says how to fix each problem.

### In a container

```sh
make container                    # podman build (CONTAINER=docker for Docker)
make container-run                # 127.0.0.1:7340, data in the cerberus-data volume
podman exec cerberus cerberus url # sign-in link
```

For a systemd service with Ollama beside it, use the Quadlet units in `packaging/container/`. See
[docs/CONTAINER.md](docs/CONTAINER.md).

## Phone access

Cerberus binds to loopback only. To use it from a phone, add a private address, such as your
Tailscale IP, and pair the phone.

```sh
cerberus config set server.extra_listen '["100.64.0.10:7340"]'
cerberus config restart
cerberus pair --name phone        # a token for this phone only, a link and a QR code, shown once
```

- `cerberus init` can do this for you when it finds Tailscale.
- Each paired device has its own token. Revoke a lost phone with `cerberus devices rm phone` or in
  Settings › Security. Its live streams end at once.
- Cerberus refuses a public listen address unless `server.allow_public` is set. Don't set it.

**HTTPS.** Browsers allow the microphone and app install only on a secure page, so a phone needs
HTTPS. With Tailscale:

```sh
cd ~/.config/cerberus
sudo tailscale cert --cert-file desk.crt --key-file desk.key desk.your-tailnet.ts.net
sudo chown "$USER" desk.crt desk.key && chmod 600 desk.key
```

```json
"server": { "extra_listen": ["100.64.0.10:7340"],
            "tls_cert": "/home/me/.config/cerberus/desk.crt",
            "tls_key":  "/home/me/.config/cerberus/desk.key",
            "public_url": "https://desk.your-tailnet.ts.net:7340" }
```

- Every `extra_listen` address then serves HTTPS. The loopback address stays plain HTTP for desktop
  clients. The login cookie is marked `Secure`.
- Open the page by its `ts.net` name so the certificate matches.
- The certificate reloads on SIGHUP and when its files change (checked hourly). Re-running the same
  `tailscale cert` command before it expires is enough.
- An extra address that is not up at boot is retried in the background until it binds.
- `cerberus init` prints the `tailscale cert` command. It never runs it.

The UI opens on Chat. On a phone it has four bottom tabs: Chat, Sessions, Monitor and More (Models,
Usage, Settings). Secrets and About (version and build) are groups in Settings. Over HTTPS it
can be installed as an app.

## Features

### Models

- **Engines:** llama.cpp (Cerberus starts `llama-server` itself), LM Studio and Ollama. Cerberus
  never runs inference itself.
- **On demand:** a request for a model that is not loaded loads it. Idle models unload after
  `engines.idle_ttl_sec`. Before a load, idle models are unloaded (least recently used first) to make
  room in VRAM.
- **Model IDs:** local models use their backend ID; cloud models are `<provider>/<model>`. `default`
  means `engines.default_model`; while that is unset it means the only loaded model, or the only
  model there is. A unique prefix or suffix of an ID also works.
- **Metrics:** each request records time to first token, tokens per second, tokens and, for local
  models, energy from the GPU's power readings. The **Models** view shows them per model, with 24 h
  and month energy and cost.
- **Get models:** Models › **Get models** (or `cerberus pull`) downloads an Ollama model, an LM
  Studio model (`lms get NAME --yes`, which picks the catalogue's best match) or a Hugging Face
  `.gguf` into the first llama.cpp models folder. GGUF downloads resume, check the SHA-256 when one
  is given, and are refused when the disk would fill. One download per engine runs at a time; the
  rest queue. Nothing is ever loaded.
- **Monitor:** the whole machine. Watts at the wall split into CPU, GPU and the rest; kWh and cost
  today, this month, projected and over 30 days at your marginal tariff tier (CFE Tarifa 1 by
  default, DAC risk included); 1 hour, 7 day and 30 day charts; per-thread CPU, memory, network,
  GPU and drives; processes grouped by app, which you can renice or stop (system, desktop and
  Cerberus processes are locked). Alerts cover disk fill, temperatures, sustained power, the
  projected bill and DAC risk.
- **Power readings:** GPU watts are measured. CPU watts come from RAPL when readable (see
  `cerberus rapl-setup`), else from a load model. The rest is `monitor.baseline_w` plus 5 W per
  spinning disk.
- **Terminal:** `cerberus top` is a live terminal view; `cerberus energy` prints the totals and the
  bill estimate.

### Gateway

- **One endpoint:** `http://127.0.0.1:7340/v1`, OpenAI-compatible, streaming included. From the same
  machine it needs no key (`server.local_no_auth`); from elsewhere it needs a token.
- **Providers:** any OpenAI-compatible cloud API under `providers`. Keys come from the provider's
  `key_env`, then the vault. The client's own `Authorization` header is never sent upstream.
- **Vault:** `cerberus secret set NAME` or **Settings → Secrets** stores keys. Values are write-only:
  no API, log or page returns them. It uses the desktop keyring through `secret-tool` when one
  answers, else an encrypted file (`vault.json`, which protects against accidental leaks, not against
  someone who can read your files). Sessions get secrets as environment variables through
  `secret_env`, and every vault value is masked as `••••••` in terminal output, transcripts and
  recordings.
- **Spend:** set prices (USD per million tokens) to see cloud spend per provider in **Usage**. Past a
  budget's `hard_usd` the gateway answers `429 budget_exceeded`; `soft_usd` sends one notification per
  period. Local models always cost 0.
- **Subscriptions** used through an official CLI are only observed. For Claude Code, Cerberus reads
  the CLI's local transcripts and shows tokens in the current 5-hour block and the rolling week
  against limits you estimate yourself. Nothing is enforced.

```json
"prices":  { "openrouter/*": { "in": 3, "out": 15, "cached_in": 0.3 } },
"budgets": [ { "provider": "openrouter", "period": "month", "soft_usd": 20, "hard_usd": 30 } ]
```

### Agents

A session is one agent run in one workspace:
`queued → starting → running ⇄ waiting → checking → passed | failed`, then `accepted`, `discarded`,
`stopped` or `error`.

- **Workspaces:**
  - A git repo (its top level) is a **repo** session in a detached worktree, with a diff, checks and
    accept.
  - Any other folder is a **folder** session. The agent edits it in place and there is no diff or
    accept. Discarding forgets the session; Cerberus never deletes your folder.
  - No folder is a **scratch** session in an empty folder Cerberus owns. Save its files to a folder
    or download a zip at any time. Discarding deletes it.
  - Folders must be inside your home, not on `agent.deny_paths`, and not Cerberus' own data.
- **Profiles:** `cerberus` (the built-in agent through the gateway, any model), `claude` and
  `claude-headless`, `opencode`, `aider` and `shell`. A profile is a plain argv template; add your
  own under `agents`. A profile whose binary is missing shows as unavailable.
- **Approvals:** the built-in agent asks before it runs a command or writes a file. The session turns
  `waiting`, a notification goes out, and you answer from the UI. Paths on `agent.deny_paths` are
  always refused.
- **Saved permissions:** "Always ▾ → Every time" on an approval, or "Always allow" on a chat card, saves
  a narrow rule (an exact command, a safe prefix such as `go test:*`, a folder or a host) that answers
  your later chats and sessions without asking. See, add and remove them in **Settings → Permissions**.
- **Work previews:** a session's **Preview** tab (`#/s/<id>/preview`) shows the site it is building:
  a static site (an `index.html` in the root, `public/`, `dist/`, `build/`, `out/`, `site/`, `_site/`
  or `docs/`, or the folder `.cerberus.json` names), a dev server the agent started (found by port,
  only if the session's own processes listen on it), or **Run preview**, which starts the command
  from `.cerberus.json` (`{"preview":{"command":"npm run dev -- --port {port}"}}`) or the
  `package.json` dev script in the session's sandbox. Phone, tablet and desktop widths, reload, open in
  a new window, copy link, the server's log; a chip on the session row and wall tile. With
  `server.preview_ports` (e.g. `7350-7369`) each preview gets its own origin, so absolute paths, client
  routing and hot reload work; viewers can watch, only the owner or an admin starts and stops.
- **Test gate:** when the agent exits, the repo's checks run (from `repos` in the config or the repo's
  own `.cerberus.json`). On failure the agent gets another attempt with the output. After
  `max_attempts` the task can escalate to another profile.
- **Accept:** squashes the work into one commit with your identity, rebases it onto the current
  `main`, fast-forwards `main` in your checkout, and optionally pushes. Cerberus never creates a
  branch.
- **Guard and queue:** a session starts only while running sessions, free RAM and (for a local model)
  free VRAM allow it. Otherwise it waits in the queue with the reason shown. With systemd each session
  runs in its own scope under `agents.slice`; without a user bus it runs without one.
- **History:** every session keeps a plain-text transcript. Search titles, tasks and transcripts from
  **Sessions** or with `cerberus search`. **Resume** starts a new attempt in the same workspace.
  Finished sessions older than `sessions.retention_days` (30) are pruned daily, except worktrees with
  unaccepted changes.
- **Fleet tasks:** one instruction, one session per repo under `repo_roots` that has a given file or
  matches a name. **Accept N passed** accepts them all, one commit each.
- **Voice:** the mic button transcribes speech on this machine (Atrium's voice script, `whisper-cli`
  or a Whisper URL you set) and inserts the text for you to review. It never sends by itself.
- **Recording and replay:** each run is recorded as an asciicast (masked like the terminal). The
  session's **Replay** tab plays it at 1× to 16× with a scrubber and links to a moment.

```sh
cerberus task "Fix the failing test"                       # repo session for the repo you are in
cerberus task --agent claude --dir ~/notes "Tidy the index"
cerberus fleet --has carino-navbar.js "Update the navbar in {name} to the shared version"
```

### Control center

Watch and steer many agents at once, and let agents start helpers within limits.

- **Wall** (in Sessions, `#/wall`, `g w`): live mini terminals for the active sessions over one
  stream. **Active** shows them as a grid of small tiles above only the running and waiting
  sessions; **All** lists every session with the tiles as one short strip. A visit starts on Active
  when something runs, else All. Tap a tile to fill the screen and type into it. `#/wall/<id>,<id>` (the bulk bar's Wall button) is a
  full-screen watch page for a chosen set.
- **Sessions list:** select several sessions and send input, follow-ups or keys to all of them, or
  stop, resume, accept, discard, re-check, tag, pin or reprioritise them together. Rename inline.
  Spawned sessions sit under their parent.
- **Limits** per session: a time limit and an output limit per run, and a memory cap for the systemd
  scope. **Kill** ends an agent that ignores Stop.
- **Activity feed:** every state change and action, filterable by kind. Typed input is clipped, and
  hidden input (a password prompt) is never logged.
- **Keyboard** (desktop): `g w` wall, `g s` sessions, `g c` chat, `n` new task, `/` search, `j`/`k`
  move, `x` select, `Enter` open, `.` bulk bar, `?` help.
- **Agents managing agents:** each session gets its own agent key. With it an agent can start child
  sessions and see, read, wait for and stop only its own descendants, through `cerberus mcp` (six
  `cerberus_*` MCP tools) or the built-in agent's own tools. `agent.spawn` limits depth, children and
  tree size, and can require your approval for each child.

### Delegation, Flow and fine-tuning

- **Delegation:** an agent in a session can hand bulk work (summaries, searches, first drafts) to a
  local model with `cerberus_ask` and rate the answer with `cerberus_rate`, both through
  `cerberus mcp`. Usage › **Delegated to local models** shows the jobs, tokens and what they saved.
- **Flow:** a session's **Flow** tab (`#/s/<id>/flow`) draws the whole session tree as a live graph:
  you, the sessions and their spawned children, and the models each one asked (dashed) or used
  itself (dotted), with job, request and rating counts on each line. Drag to pan, pinch or wheel to
  zoom, double-tap to fit; tap a session to open it, a model to open Models. On a phone the graph
  runs top to bottom. A chat's **Flow** button shows the same for that chat. It refreshes on session
  and usage events, at most every 2 s, and never while the page is hidden.
- **Fine-tuning:** with `finetune.capture` on (`delegated`, or `cloud` to also keep cloud chat
  answers), every delegated job is kept as a training example. Settings › **Fine-tuning** lists
  them 20 at a time with filters and search; mark each **Good** or **Bad**, write a **Correct…**
  answer (your rating wins over an agent's), or **Exclude** it from training. **Train now** exports
  the usable rows into a new run folder and runs your `finetune.train_command` as a scratch session,
  so you get its terminal, replay, notifications and Stop. Past runs are listed with their state.
  Cerberus never ships a trainer and never trains on its own.

### Flows

A **flow** is a saved recipe: a trigger and a list of steps that hand work to models and agent
sessions. The **Flows** tab (after Sessions; on a phone in More) lists them with their trigger in
words, next and last run, an on/off switch and **Run**.

- **Steps:** `prompt` (one chat completion, text or JSON), `plan` (a JSON list of parts), `fan_out`
  (each part to the model its kind routes to, or to a session, a few at once), `session` (an agent
  session, never auto-accepted), `if` (contains, regex, equals, JSON path or session state; jumps
  forward), `approve` (waits for you: Approve / Reject in the notification list or on the run page)
  and `output` (a notification, an inbox entry or a file in the run folder). Text fields are
  templates with `{input}`, `{steps.<id>.output}`, `{item.title}`, `{date}` and friends; unknown
  placeholders are refused when you save.
- **Triggers:** manual (button, CLI, API) or a schedule: `every 30m`, `daily 08:00`,
  `weekdays 08:00`, `mon,thu 19:30` or a cron line, in local time. Missed runs are not caught up
  beyond one run; a scheduled run is skipped (and says so) when the guard refuses or the last run
  is still going.
- **Editor:** name, trigger with a friendly picker, input hint, limits, and the steps as cards with
  model, agent and repo pickers, beside a live graph preview; problems show on the field. **Test
  run** runs the draft and streams each step. Start from **Clarify → Answer**, **Draft → Critique →
  Revise**, **Plan → Delegate → Merge** or **Website**.
- **Runs:** at most `flows.max_runs` (2) at once, each capped by steps, tokens, cost, parallelism and
  time (`limited` with the reason). The run page shows the live graph, each step's input and output,
  tokens, cost and energy, and links to the sessions it started. Model calls go through the gateway
  as client `cerberus-flow` with the run id as session, so Usage counts them. Starts, approvals,
  cancels and flow changes are audited. Flows live in `DATA/flows/` (the last 10 versions kept).

### Files

Give files to agents and take files back, from a desktop or a phone.

- **In:** drag files onto the window, use the paperclip on the input bar (camera and gallery on a
  phone), **Add files** on the New task form, or paste. Files land in the workspace's `inbox/`
  (`files.upload_dir`). A repo session's inbox is excluded from git, so uploads never reach an
  accepted commit by accident.
- **Files tab:** browse the workspace with previews of images, text, code, PDF, audio and video;
  download, delete, make folders, and **Send to agent**.
- **What agents see:** the built-in agent is told about new inbox files and gets new images as image
  parts when the model takes them. Other agents get "Files added: …" with the next follow-up.
- **Carino Bridge:** other Carino pages can hand files to a session or a new task, and **Open in
  Carino Files** sends one out.
- **Limits:** names are cleaned, every path stays inside the workspace, symlinks out are never
  followed, `.git` is hidden, uploads are never executable unless `files.allow_exec`, and size limits
  (`max_file_mib`, `max_total_mib`) are checked while streaming.

### Safety

Cerberus runs commands as you. Treat access to it like an SSH login. The full threat model is in
[docs/SECURITY.md](docs/SECURITY.md).

- **Private by default:** loopback only; extra addresses are opt-in; a public address is refused
  unless `server.allow_public`. Every request's `Host` must be one of ours (stops DNS rebinding).
- **One token, plus devices:** a bearer token protects everything except the login page, the health
  check, static assets and the app manifest and service worker. Paired devices have their own tokens. `cerberus token rotate` replaces
  the admin token.
- **CSRF:** cookie-authenticated writes must carry `X-Cerberus: 1`, which a cross-site page cannot
  send.
- **Rate limit:** 5 wrong credentials a minute from one address block that address's bad attempts for
  10 minutes. Valid credentials still work.
- **Agents never hold the admin token.** Each session's key works only for `/v1` and its own
  subtree of sessions.
- **Sandbox:** set `sandbox.default` (or per profile or repo) to `bwrap` to run agents in bubblewrap:
  `/` read-only, `~/.ssh`, keyrings, browser profiles and Cerberus' own folders hidden, the workspace
  writable, network optional. Inside it git commits fail; accept commits on the host.
- **Command policy:** the built-in agent checks each command against `agent.policy`. Dangerous
  commands are refused; risky ones (`git push`, `sudo`, `rm -r`) are always asked. It is a guard rail,
  not a sandbox.
- **Audit log:** every state-changing call, auth failure, rotation, pairing, sandbox fallback and
  process signal goes to `DATA/audit.jsonl`, with secrets masked. Read it with `cerberus audit` or in
  Settings › Security.
- **Private files:** everything Cerberus writes under `DATA` and `~/.config/cerberus` is 0600 in
  0700 folders, written atomically (temp file, then rename). Staged uploads and attachments too;
  files delivered into a workspace get normal modes, and model downloads are 0600.
- **Tested as a matrix:** a test lists every route from the server's own mux and checks each one
  for auth, CSRF, the agent scope and exactly one audit record per write.
- **Doctor:** `cerberus doctor`, a card on Monitor and Settings › Security check file modes, listen
  addresses, TLS, token age, the sandbox and more.
- **Previews** of agent-written pages run on an origin of their own (another port, or a sandboxed
  opaque origin without preview ports), enter through a one-time capability that never reuses your
  sign-in, and cannot call the API with your cookie. Dev servers are proxied only when the session's own
  processes listen on the port, and never see Cerberus' cookies.

### LLM chat

The **Chat** view (`#/chat`, `g c`) talks to any model Cerberus knows. The chat runs on the server,
so closing the tab does not stop a reply, and another browser picks up where it is.

- **Tools:** the model can read files, search and run commands in the chat's folder (your home by
  default). Anything that changes something shows a permission card first: allow once, allow for this
  chat, or deny with a note.
- **Modes:** Ask me, Auto (reads and safe read-only commands run), Accept edits, Plan (read-only). The
  command policy and deny paths apply in every mode.
- **Models:** local models, API providers, and the `claude` CLI as an optional backend when it is
  installed.
- **Conversations:** messages queue while a reply runs; Stop ends the reply and drops the queue.
  Images are scaled in the browser. The status line shows context use, time and cost.
- **Quick answer:** the `??` box asks one short question without tools.
- **Recent chats** (Ctrl+O) lists Cerberus chats, recent `claude` sessions and the desktop chat's
  saved sessions, which open read-only; **Continue here** copies one in.
- **Mascots:** an animated text-art mascot reacts to the chat, in 11 themes (Ctrl+J), ported from
  the desktop chat. It stops animating while nothing happens or the tab is hidden.

Chats live in `DATA/chats/` (0600). Settings are under `chat.*`.

### Extras

- **Inbox:** every notification (session finished, waiting or failed; chat done; budget; alert;
  download done) is also kept in the bell's panel, even with outside notifications off. Desktop
  alerts are opt-in.
- **Install as an app:** the UI has a web manifest and a service worker that caches only the app
  shell. Offline, it opens with a Retry button. It needs HTTPS or loopback.
- **Leaderboard:** Usage › **Leaderboard** ranks sessions, agents, models or repos by cost, tokens,
  energy or run time, over today, 7 days, 30 days or this month.
- **Product page:** the repo root (`index.html` and `site/`) is the page for cerberus.carino.systems.
  The daemon never serves it. See [docs/SITE.md](docs/SITE.md).

## Configuration

`~/.config/cerberus/config.json` (0600). Every field has a label, help, group, level (basic or
advanced), limits and a restart flag in the schema: see `cerberus config schema`, or the **Settings**
view.

- **Settings view:** groups, basic fields first with an Advanced switch, search, list editors for
  agents, repos, providers, budgets and tariff tiers, and a Save bar. A refused save keeps your draft
  and shows each problem under its field. Settings › Security holds devices, token rotation, the
  doctor and the audit log.
- **CLI:** `cerberus config get|set|reset|edit|validate|schema|path|restart`, by path:
  `engines.idle_ttl_sec`, `agents[0].command`, `prices["qwen2.5"].in`.
- **Validation:** addresses, ports, ranges, enums, references and duplicates are checked. A change is
  saved all or nothing.
- **Live reload:** most changes apply at once. Listen addresses, TLS, engine backends, providers, the
  vault and voice need a restart: Settings shows **Restart now**, or run `cerberus config restart`.
- **Profiles:** `cerberus export` writes agents, repos, providers (without keys), prices, budgets,
  tariff, monitor, limits, sandbox, policy and files as JSON. `cerberus import` shows the diff and
  asks first. Secret values never leave.

The fields most people change:

```json
{
  "server": { "listen": "127.0.0.1:7340", "extra_listen": ["100.64.0.10:7340"], "public_url": "" },
  "engines": { "default_model": "qwen3-coder-30b", "idle_ttl_sec": 600,
               "llamacpp": { "model_dirs": ["/home/me/models"] } },
  "providers": [ { "name": "openrouter", "base_url": "https://openrouter.ai/api/v1",
                   "key_env": "OPENROUTER_API_KEY", "enabled": true } ],
  "repo_roots": ["/home/me/Github"],
  "repos": [ { "path": "/home/me/Github/site", "checks": ["make test"], "preview": { "dir": "." } } ],
  "guard": { "max_sessions": 3, "min_free_mib": 4096 },
  "git": { "name": "Your Name", "email": "you@example.com" }
}
```

Data lives in `~/.local/share/cerberus/` (DATA): `sessions/`, `worktrees/`, `chats/`, `energy/`,
`usage.jsonl`, `audit.jsonl`, `inbox.jsonl`, `flows/` (flows, versions, runs and `schedule.json`), `logs/`. Flows are switched by `flows.enabled` and capped by `flows.max_runs` (advanced).

## CLI

Run `cerberus help` for the overview and `cerberus help COMMAND` (or `cerberus COMMAND -h`) for one
command. Every command that reads the config takes `-config PATH`.

| Command | What it does |
|---|---|
| `init [--yes] [--json]` | First-run setup: detect, ask at most 6 questions, write the config. |
| `serve [-debug]` | Run the daemon. SIGTERM shuts down in order; SIGHUP reloads the TLS certificate. |
| `install-service` | Install and start the systemd user unit (Linux). |
| `url` | Sign-in links with the token for every listen address. |
| `status` | One-screen summary of the running daemon. |
| `doctor [--json]` | Safety and health checklist; exit 1 on a failed check. |
| `version` | Version, OS/arch and Go version. |
| `task [--dir D \| --repo R \| --scratch] [--agent A] [--model M] [--escalate P] [--count N] [--title T] "task"` | Start a session and print its link. |
| `search [--repo --agent --state --since --until --limit] "words"` | Search session history. |
| `resume ID ["text"]` | New attempt of a finished session in its workspace. |
| `fleet --has FILE \| --name GLOB [--yes …] "task"` | One session per matching repo; `{name}` and `{repo}` expand. |
| `prune` | Delete old finished sessions now. |
| `pull [--engine E] [--sha256 H] [--detach] REF` / `pull --cancel ID` | Download a model without loading it. `--sha256` only for GGUF. |
| `top [--once] [--every 2s]` | Live terminal view of the machine and sessions. |
| `energy` | Energy totals and the bill estimate. |
| `rapl-setup` | Print the one-time root steps for CPU watts; never runs them. |
| `secret list \| set NAME [--note T] \| rm NAME` | Manage the vault; `set` reads the value from stdin. |
| `pair [--name N] [--url BASE] [--light-terminal]` | Pair a device: its own token, a link and a QR code. |
| `devices [list \| rm ID\|NAME]` | List or revoke paired devices. |
| `token rotate` | Replace the admin token. |
| `audit [--since 24h] [--actor X] [--limit N] [--json]` | Read the audit log. |
| `config path \| get \| set \| reset \| edit \| validate \| schema \| restart` | Read and change settings by path. |
| `export [--secrets-names] [--sections S] [-o FILE]` / `import [--yes] FILE` | Profile bundles. |
| `agent …` | The built-in coding agent. Sessions run it. |
| `flow list \| show ID \| run ID [--input T] [--wait] \| runs [ID] [--limit N] \| cancel RUN \| approve RUN [--reject] [--note T]` | Flows through the daemon; `run --wait` follows the run to its end or its approval. |
| `mcp [--url --key --session]` | Stdio MCP server for agents in a session (MCP 2026-07-28 and 2025-06-18; `initialize` optional, `server/discover` answered). Sessions run it. |
| `finetune [stats] \| export [--out F --kind K --unrated] \| train [--dry-run --base M]` | Training examples: counts, chat JSONL, or run `finetune.train_command` in the terminal. |

## Desktop

Atrium's LLM chat works with Cerberus unchanged:

```sh
~/.config/hypr/scripts/llm_models.py set server_url http://127.0.0.1:7340/v1
```

No key is needed from the same machine. Session notifications go through Atrium's
`~/.config/hypr/scripts/notify` when it exists, else `notify-send`, plus ntfy when configured.
Details and the desktop client plan are in [docs/DESKTOP.md](docs/DESKTOP.md).

## Resource use

All energy goes to the models. The control plane does not poll while nobody watches: the GPU monitor
slows down when nothing is loaded, the scheduler and model reaper block until there is work, the
system monitor takes one cheap sample every 30 s, and the web UI closes its streams while the tab is
hidden. Measured on Linux with no model loaded and every part in use (vault, subscription tracking,
voice, HTTPS, monitor, chats, inbox, downloads, recordings): about **13 to 22 MB RSS and no measurable
CPU time** over 20 s idle windows. The day phase 7 recheck, a fresh install after sessions, chats and
uploads, with no browser open: **18 to 22 MB RSS and 0.01 to 0.02 s of CPU in 60 s** (under
0.04 %), all of it the 30 s monitor sample.

## Development

```sh
make test       # go test -race ./... (the race detector needs cgo; the binary does not)
make vet        # go vet for linux, darwin and windows
make test-web   # node --test for the UI helpers, node --check on every UI module
make cross      # static binaries for linux, darwin and windows, amd64 and arm64
python3 web/tools/build_sw.py    # regenerate web/dist/sw.js after any change under web/dist
python3 web/tools/make_icons.py  # app icons from the Cerberus mark (needs rsvg-convert, Pillow)
```

Dependencies are the Go standard library and `github.com/creack/pty`. The web UI is vanilla ES
modules with no build step. xterm.js and the fonts are vendored under `web/dist/assets/`, never
loaded from a CDN.

## Documents

- [docs/SCOPE.md](docs/SCOPE.md): the specification, phase by phase, with what was really built.
- [docs/SECURITY.md](docs/SECURITY.md): the threat model and what Cerberus does not protect against.
- [docs/DESKTOP.md](docs/DESKTOP.md): Atrium hookup and the desktop client plan.
- [docs/SITE.md](docs/SITE.md): the cerberus.carino.systems product page.
- [docs/USERS.md](docs/USERS.md): user accounts and roles, running Cerberus for several people.
- [docs/CONTAINER.md](docs/CONTAINER.md): the container image, Quadlet units and compose file.

## Licensing

**Mine — GNU Affero General Public License v3.0 or later.** Everything in this
repository *except* the paths listed below. Copyright © 2026 Miguel Carino.
Full terms in [LICENSE](LICENSE). If you run a changed Cerberus as a service
other people use over a network, the licence asks you to offer them its source.

**Not mine.** The files below are third-party works redistributed here. This
project's licence does not cover them and could not: they are not mine to
relicense. Each keeps its own terms, and each carries its own notice.

| Path | What it is | Licence | Notice |
| --- | --- | --- | --- |
| [`site/fonts/`](site/fonts/), [`web/dist/assets/fonts/`](web/dist/assets/fonts/) | IBM Plex Mono, IBM Plex Sans, Red Hat Display | SIL OFL 1.1 | `OFL.txt` in each folder |
| [`web/dist/assets/vendor/xterm/`](web/dist/assets/vendor/xterm/) | xterm.js | MIT | [`LICENSE`](web/dist/assets/vendor/xterm/LICENSE) |
| [`web/dist/assets/vendor/addon-fit/`](web/dist/assets/vendor/addon-fit/) | xterm.js addon-fit | MIT | [`LICENSE`](web/dist/assets/vendor/addon-fit/LICENSE) |

The Go build also links [creack/pty](https://github.com/creack/pty) (MIT), which
is fetched as a module and not kept in this repository. Those files travel with
any fork, mirror or repackaging, and their notices must travel with them.

## Phases

| Phase | Contents |
|---|---|
| 1 (done) | Model hosting and gateway; agent sessions with worktrees, test gates, previews, a resource guard and queue, escalation and remote approval; web UI; CLI; systemd user unit |
| 2 (done) | Secrets vault; spend, budgets and subscription gauges; session history, resume and retention; fleet tasks; voice; HTTPS for phones |
| Day plan (done) | Monitor; control center; file drop; safety; startup and configuration; LLM chat with mascots; extras; hardening and documentation |
| After the day plan (done) | Model problems in the chat; delegation to local models and training examples; navigation, Settings and versions; Flow view and fine-tuning |
| Flows (done) | Designed, scheduled and triggered flows: engine, API, CLI, the Flows view and templates; MCP 2026-07-28 |
| 3 | GPU power-cap auto-tuner (tokens per joule), speculative-decoding and quantisation presets, vLLM backend, several GPUs and hosts |
| 4 (client edition) | Plain chat for non-developers (Spanish first), RAG over documents, users and roles, fully offline, remote support |
| 5 (desktop) | Desktop client grown from Atrium LLM chat, island agent status, launcher "new task", bar readout of GPU power and tok/s; macOS and Windows builds |
