# Cerberus on the desktop

Cerberus is a service plus a desktop client. The service is the `cerberus` daemon. The desktop base
is Atrium's LLM chat (`llm-chat.py` with `llm_chat_engine.py` and `llm_models.py`), which already
speaks the OpenAI API. Phase 1 connects the two without changing Atrium's code. Phase 5 grows that
chat into the Cerberus desktop client.

## Phase 1: hookup

### Run the service

```sh
cerberus install-service     # ~/.local/bin/cerberus + systemd user unit, enabled and started
systemctl --user status cerberus
```

The unit starts with the user session, restarts on failure and stops gracefully. On stop, Cerberus
ends its sessions and shuts down any `llama-server` it spawned. The unit sets
`CERBERUS_SUPERVISOR=systemd` and `RestartForceExitStatus=75`, so **Restart now** in Settings (or
`cerberus config restart`) exits 75 and systemd starts it again. An older unit file lacks these lines:
run `cerberus install-service` again to update it.

Agent binaries (`claude`, `aider`, `opencode`…) must be on the service's PATH. The systemd user
manager starts with a bare PATH, so the unit sets `~/.local/bin:~/bin:/usr/local/bin:/usr/bin:/bin`
and `cerberus serve` also adds `~/.local/bin` and `~/bin` when they exist. An agent installed
elsewhere needs a full path in its profile's `command`, or a `systemctl --user edit cerberus`
override that extends `Environment=PATH=…`.

### Point Atrium LLM chat at Cerberus

```sh
~/.config/hypr/scripts/llm_models.py set server_url http://127.0.0.1:7340/v1
```

Settings › AI & models sets the same key in `~/.local/state/hypr/llm.json`. Leave `server_key`
empty: with `server.local_no_auth` (the default), `/v1/*` from the same machine needs no token. From
another machine, set `server_key` to the Cerberus token and use an address from `extra_listen`.

What this gives the chat:

- **One model list.** Cerberus' models appear as `server:<id>` entries: local models from llama.cpp,
  LM Studio and Ollama, plus `<provider>/<model>` for every enabled cloud provider. `default` maps to
  `engines.default_model`.
- **Load on demand.** Picking a model that isn't loaded makes Cerberus load it, after making room in
  VRAM by unloading idle models, least recently used first. Idle models unload after their TTL.
- **Metrics.** Each request is recorded with time to first token, tokens per second, tokens and, for
  local models, energy from the GPU's power readings. They show in the web UI under Models and Usage.
  The chat keeps receiving the usage chunk it asks for with `stream_options.include_usage`.

Atrium's own `local` kind still talks to LM Studio directly. Cerberus sees models loaded that way as
loaded by someone else and leaves their unloading to LM Studio's TTL; it can still evict them when it
needs room. Using only `server:` models gives Cerberus the full picture.

Optional: a client may send `X-Cerberus-Client: <name>` (and `X-Cerberus-Session: <id>`) so its
requests are attributed in the usage log. Atrium does not send it today; its requests are recorded
with an empty client.

### Notifications

When `~/.config/hypr/scripts/notify` exists, Cerberus uses it for session events (waiting for
approval, passed, failed, escalated, error), so they get Atrium's styling and appear in the dynamic
island. Without it, Cerberus falls back to `notify-send`. `notify.ntfy_url` adds phone push through
ntfy. Sends are rate-limited to one per session and state every 10 s.

Every notification is also kept in the web UI's inbox (the bell), even with `notify.disabled`. A
desktop client can read it from `GET /api/inbox` and follow `inbox` events on `/api/events`.

### Agent tasks from the desktop

The CLI and the web UI are the desktop entry points until the Phase 5 client exists. The web UI can
be installed as an app from the browser (loopback or HTTPS only), which gives it its own window:

```sh
cd ~/Github/some-repo && cerberus task --agent cerberus --model default "Fix the failing test"
cerberus url      # open the session on a phone: live terminal, approvals, diff, accept
```

## Phase 5: the desktop client

The desktop client grows out of Atrium's LLM chat. It stays a native client of the same HTTP API the
web UI uses, with no embedded browser runtime and no second copy of any logic.

| Piece | Source of data | Behaviour |
|---|---|---|
| Chat | `/v1` (unchanged) | Model picker grouped by local, loaded and cloud; per-reply tok/s and energy from the usage log. |
| Island agent status | `GET /api/events` (SSE) | Shows running and waiting sessions; a waiting approval opens a small y / a / n prompt that posts to `/api/sessions/{id}/input`. |
| Launcher "new task" | `GET /api/repos`, `GET /api/status` (agents), `POST /api/sessions` | Repo picker (recent first), task text, agent and model; opens the session view or the web UI. |
| Bar readout | `GET /api/status` (gpu, models) | GPU power and the current tok/s while a model is generating; hidden when idle. |
| Session view | `GET /api/sessions/{id}/stream`, `/diff` | Terminal and diff review with accept and discard, for when a phone isn't at hand. |

The web UI already carries a browser version of this chat (day phase 5, the Chat tab): conversations
run in the daemon through `/api/chats`, so a desktop client can show the same chats and permission
cards instead of running tools itself. The mascots are ES modules under
`web/dist/assets/js/mascots/`, checked frame for frame against the desktop originals.

The Chat tab also lists the desktop chat's saved sessions (`$XDG_STATE_HOME/hypr/llm-chat/sessions/`,
switch: `chat.atrium_import`). They open read-only; **Continue here** copies one into a Cerberus chat
and never writes the desktop files. The `claude` CLI, when installed, is one optional chat backend;
everything else works without it.

Energy rules carry over from the service: one SSE connection for events while visible, nothing while
hidden, no timers when no model is loaded. The client authenticates with the bearer token read from
`~/.config/cerberus/config.json`; it never needs the cookie or the CSRF header.

### macOS and Windows

The service already cross-compiles for both. Platform paths are in place (`~/Library/Application
Support/Cerberus`, `%LOCALAPPDATA%\Cerberus`), as are process-group signalling behind build tags and
stubs where something isn't supported yet. What remains:

- **Sessions on Windows:** a ConPTY implementation of `platform.StartPTY`, and process-tree
  termination through Job Objects instead of signals. The built-in agent's `run_command` needs a
  shell other than bash, or Git Bash.
- **Service install:** a launchd agent on macOS and a per-user Windows service or scheduled task,
  behind `cerberus install-service`.
- **GPU readings:** `nvidia-smi` works on Windows as on Linux. Apple Silicon needs a power source that
  does not require root (or reports energy as unknown, which the gateway already handles).
- **Engines:** LM Studio and Ollama work as on Linux; `llama-server` binaries exist for both.
- **Desktop client:** the Atrium-based client is Linux and Hyprland specific today; the island and
  bar pieces need native equivalents (menu bar item on macOS, tray on Windows).
