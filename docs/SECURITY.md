# Cerberus security

This is the threat model for Cerberus. It says what Cerberus protects, from whom, how, and where it stops.
The contract it follows is "Safety specification (day phase 3)" in [SCOPE.md](SCOPE.md).

Cerberus runs commands on its host for you. Treat it like an SSH login: anyone who can act as you in Cerberus
can do what your user account can do.

## Summary for the owner

- Keep Cerberus on loopback, your LAN or your tailnet. Never put it on the public internet.
- Use TLS (for example `tailscale cert`) for anything that is not loopback.
- Pair each phone or laptop (`cerberus pair --name phone`) instead of sharing the admin token. Revoke lost devices.
- Rotate the admin token (`cerberus token rotate`) if it may have leaked.
- Turn on the bwrap sandbox for agents that run model output (`Sandbox.Default: "bwrap"`, or per profile or repo).
- Use the file vault backend only when no desktop keyring exists, and know what it does not protect.
- Run `cerberus doctor` after changing the config, and fix every "fail".
- Read the audit log now and then (`cerberus audit --since 24h`, or Security in the web UI).

## Assets

| Asset | Why it matters |
|---|---|
| The host account | Agents run as your user. Your files, keys, browser profiles and mail are all reachable from it. |
| Repos and folders | Agents edit them. A bad accept or push reaches your remotes. |
| Secrets in the vault | Provider API keys and tokens. They cost money and open other services. |
| The admin token and device tokens | Either one is full control of Cerberus, and so of the host account. |
| Model and API spend | Remote models bill per token. A runaway agent or a stolen key spends your money. |
| Other LAN devices | An agent with network access can reach your printer, router, NAS and other hosts. |

## Actors

| Actor | Trust |
|---|---|
| The owner | Trusted. Uses the admin token, the CLI or the desktop. |
| Paired devices | Trusted, one by one. Each has its own token and can be revoked. A device acts with its user's role. |
| Signed-in users (Phase 4a) | Trusted per role: admins fully; `user`, `viewer`, `monitor` and `chat` only within their role and their own data (see "Users and roles" below and [USERS.md](USERS.md)). |
| Agents | Semi-trusted. They act for the owner but run model output, which may be wrong or steered by what they read. |
| Web pages in the owner's browser | Untrusted. They may try CSRF or DNS rebinding against Cerberus. |
| Other hosts on the LAN or tailnet | Untrusted unless paired. |
| Uploaded files and preview pages | Untrusted content, written by people, agents or the web; dev servers an agent runs. |

## Entry points and controls, per actor

### The owner (admin token, CLI)

Entry points: the web UI and `/api/*` with the admin token (header or cookie), the `cerberus` CLI on the host,
the config file.

Controls:

- Bearer header, cookie or `?token=` once (it sets the cookie and redirects without the token, so it does not stay
  in the address bar). All comparisons are constant-time. See `withAuth` and `validToken` in
  [internal/api/auth.go](../internal/api/auth.go).
- The config is written atomically with mode 0600 (`Save` in [internal/config/config.go](../internal/config/config.go)).
- Token rotation replaces the admin token, rewrites the config (0600, with `server.token_created`) and ends
  cookies and live streams made from the old one (`cerberus token rotate`, `POST /api/token/rotate`;
  `rotateToken` in [internal/api/safety.go](../internal/api/safety.go)). Paired devices keep working. Session
  agent tokens derived from the old token keep `/v1` (never `/api/agent`) until their session ends; the shared
  agent token and preview capability links made from it stop at once. The new token is returned only to an
  admin bearer caller (the CLI); a browser on the admin cookie gets a fresh cookie, and a paired device never
  learns the admin token. Rotation is refused when the config has no file path.
- Every state-changing API call and CLI action goes to the audit log
  ([internal/audit/audit.go](../internal/audit/audit.go)).

Residual risk: the admin token never expires by itself (`cerberus doctor` warns after 180 days). Anyone who reads
the config file has it, and that includes an agent running without the sandbox.

Owner must: keep the config file private, rotate the token after any doubt, prefer paired devices for daily use.

### Paired devices

Entry points: the same as the owner, with a device token.

Controls:

- `cerberus pair` (or the Security view, with the admin token) creates a random 32-byte token per device. Only its
  SHA-256 hash is stored, in `devices.json` next to the config (0600, hidden from sandboxed agents), with the name,
  creation time, last use and last address. The pairing URL and QR are shown once
  ([internal/api/devices](../internal/api/devices), [internal/api/qr](../internal/api/qr)). A `devices.json` that
  cannot be read fails closed: device tokens stop, the admin token still works.
- Devices can list and revoke devices, but cannot pair new ones or rotate into the admin token.
- Revoking a device (`DELETE /api/devices/{id}` or the Security view) cancels every request it has open, so its
  SSE streams end at once. Each actor may hold at most 32 streams.
- Audit records name the device: `owner:<device>`.
- Auth failures are rate-limited per remote address (an IPv6 /64 counts as one): 5 per minute, then 429 on every
  non-public path from that address for 10 minutes, even with a good token, and logged. Only requests that present
  a wrong credential count; the same stale value counts once, and a stale cookie is cleared, so a browser left on
  an old cookie cannot lock its owner out ([internal/api/safety.go](../internal/api/safety.go)).

Residual risk: a stolen unlocked phone is the owner until it is revoked. The pairing URL holds the token, so a
screenshot or chat message of it leaks the device.

Owner must: name devices clearly, revoke lost or old ones, never send the pairing link through a chat.

### Users and roles (Phase 4a)

Entry points: the web UI and `/api/*` with a password sign-in cookie (`cls-…`), devices bound to a user,
personal `/v1` keys (`cuk-…`) and MCP clients bound to a user. See [USERS.md](USERS.md) for the guide.

Controls:

- Accounts live in `DATA/users.json` (0600, written atomically, re-read when the CLI changes it).
  Passwords are PBKDF2-HMAC-SHA256 with 600 000 iterations, a 16-byte salt per hash and a constant-time
  compare; an unknown name still runs one PBKDF2, and at most two hashes run at once. Sign-in cookies
  and personal keys are 32 random bytes stored only as SHA-256 (`DATA/logins.json`, 0600).
- `POST /api/login` needs `X-Cerberus: 1`, a JSON body and a same-origin `Sec-Fetch-Site` (no login CSRF).
  Wrong password, unknown name and disabled user give one answer. Five failures per name in 15 minutes
  lock the name (unknown names too); the per-address limiter applies as for tokens.
- Every route has a role class (`internal/api/roles.go`, fail-closed at startup; `routes_matrix_test.go`
  checks every role). Objects another user owns answer 404, never 403. `/api/events` is filtered per
  subscriber (sessions, usage hints, inbox, chat permission asks, flow runs, downloads).
- Disabling, demoting, resetting or deleting a user ends their sign-ins, revokes their devices and MCP
  clients and cuts their live requests at once (one cut on any of `user:`, `login:` or `device:` keys).
- Sessions of non-admin users always run in bwrap with a read-only home, no global `extra_rw`, and the
  data, config and runtime folders hidden; without bwrap the start is refused. They may use only scratch
  folders and repos an admin shared; accept into main needs the share's `accept`. Their chats work in
  `DATA/users/<id>/files` (Jail: no reads or writes outside it; commands only inside bwrap). Role `chat`
  has no tools. Host notifications go only to admins; others get inbox items.
- Per-user monthly budgets are enforced at every start point and on `/v1`, with spend attributed to the
  key, cookie, device, session owner or chat/run owner.
- Audit records carry the acting user's name (`user` field; `cerberus audit --user NAME`).

Residual risk: everyone's agents run as the one OS user. bwrap and path checks confine them, but a
kernel or bwrap escape, a shared CLI profile (its login is bound in) or an admin's mistake in sharing a
repo reaches the OS user's files. Do not give accounts to people you would not let use the machine.

### Agents

Entry points: the session's PTY (whatever the agent CLI runs), the gateway `/v1/*` with the per-session agent
token, the restricted agent scope `/api/agent/*`, the built-in agent's tools (`run_command`, file tools), and the
workspace files.

Controls:

- **Agent tokens.** Each session gets its own token as `OPENAI_API_KEY`. It works for `/v1/*` only and pins usage
  to its own session. It never opens the control API. See the rules at the top of
  [internal/api/auth.go](../internal/api/auth.go).
- **Agent scope.** `/api/agent/*` takes only a session token plus a matching `X-Cerberus-Session` header, and the
  session must still exist. An agent can spawn children within `Agent.Spawn` limits (depth 2, 4 children, 8 per
  tree by default, optional approval) and see or steer only its own descendants; other ids answer 404. See
  `agentAuth` in [internal/api/auth.go](../internal/api/auth.go) and
  [internal/api/agentscope.go](../internal/api/agentscope.go).
- **Workspaces.** A repo session works in its own git worktree; nothing reaches the repo until a person accepts.
  Folder pickers and file tools refuse `Agent.DenyPaths` (for example `~/.ssh`, `~/.gnupg`) and Cerberus' own
  folders ([internal/session/workspace.go](../internal/session/workspace.go),
  [internal/agent/paths.go](../internal/agent/paths.go)).
- **Resource limits.** Each run sits in a systemd-run scope under `agents.slice` when systemd is there
  ([internal/session/limits.go](../internal/session/limits.go)), and the guard refuses new sessions when memory or
  VRAM is short ([internal/session/guard.go](../internal/session/guard.go)).
- **Sandbox.** With `bwrap`, the agent sees `/` read-only, a fresh `/dev`, `/proc` and `/tmp`, its workspace and
  session folder writable, and each `Sandbox.HideHome` path (`~/.ssh`, `~/.gnupg`, `~/.config/cerberus`,
  keyrings, browser profiles) replaced by an empty tmpfs (a file is masked with `/dev/null`). Cerberus' data and
  config folders and `$XDG_RUNTIME_DIR` (D-Bus, systemd and keyring sockets) are always hidden. It runs in its
  own PID namespace and session (`--unshare-pid`, `--new-session`, `--die-with-parent`); `Network: false`
  unshares the network. Checks from `.cerberus.json` run in the same sandbox. An unknown mode word means bwrap
  (fail closed). The order is repo `.cerberus.json`, then profile, then `Sandbox.Default`; the scope is outside
  and bwrap inside. See [internal/sandbox/sandbox.go](../internal/sandbox/sandbox.go) and
  [internal/session/sandbox.go](../internal/session/sandbox.go). When bwrap is missing, the session runs
  unconfined with a feed warning and an audit `sandbox.fallback` record, or is refused with `Sandbox.Strict`.
- **Host writes into agent folders.** The session folder is writable inside the sandbox, so every host write
  into it uses a temp file and rename or `O_NOFOLLOW`, and Save-to-folder, the zip download and previews open
  workspace files through an `os.Root`, so a planted symlink cannot leak or overwrite a file outside. A worktree
  `.git` file must point back into its repo's `worktrees/` before Diff or Accept run git, so an agent cannot make
  the host run its git config. Discard removes worktrees only inside Cerberus' data folder.
- **Command policy.** The built-in agent checks each `run_command` against `Agent.Policy` deny, ask-always and
  allow lists, reading pipelines, quotes, wrappers and nested shells. Deny always wins. A pipe into a shell is
  denied; command substitution, brace expansion, a command name from a variable or glob, a pipe into an
  interpreter and unparsable lines are asked, also in auto mode. Allow skips the question only when every command
  in the line matches and none runs under sudo or a similar wrapper. See
  [internal/agent/policy.go](../internal/agent/policy.go). The built-in agent reads its policy from a 0600
  snapshot the session manager writes (`CERBERUS_AGENT_CONFIG`, no token in it), so hiding the config folder does
  not reset it.
- **Secrets in output.** Vault values are masked in session output, transcripts and the audit log
  ([internal/session/redact.go](../internal/session/redact.go), `Redact` in
  [internal/audit/audit.go](../internal/audit/audit.go)).

Residual risk:

- Without the sandbox an agent can read anything your user can, including tokens in other apps' config files.
- With the sandbox and `Network: true` the agent still reaches the LAN, the internet and abstract Unix sockets.
  With `Network: false` it cannot reach the gateway either, so local models need `Network: true`.
- Inside bwrap the home folder is read-only (tools that write `~/.cache` need `Sandbox.ExtraRW`), the repo's
  `.git` is read-only (commit on the host, with Accept), and there is no controlling terminal (`/dev/tty`, job
  control). Claude Code profiles get `~/.claude` and `~/.claude.json` writable by default.
- The agent can edit its own session files (meta, transcript, conversation). They cannot reach outside the
  session, but treat them as written by the agent.
- The policy is a guard rail, not a sandbox: a script, a Makefile or `python -c` does what it likes once it runs.
- Agents that you let push can push. Prompt injection in a README, an issue or a web page can steer an agent.
- An agent needs its provider key, so it can read that key from its own environment.

Owner must: use bwrap for agents that run unreviewed model output, keep `Approve: "ask"` for the built-in agent
on real repos, read diffs before accepting, and keep `git push` in the ask-always list.

### LLM chats (day phase 5)

A chat runs the built-in agent's tools in the daemon, as your user, in the chat's working folder, with no worktree
and no bwrap. Controls: the routes are owner-only (`chatReady` requires an owner principal; agent tokens never reach
`/api/chats`); writes need the CSRF header and are audited without message text; every write or command asks unless
the mode or a session rule covers it; the command policy and deny paths apply in every mode, and an "always ask"
command ignores session rules; reads outside the folder ask; Cerberus' own data (chat files, sessions, the config)
stays protected even when the folder is home. Chat ids are checked against a fixed pattern before any file access.
Desktop sessions are imported read-only (a copy is written, the source file never changes). Session rules live in
memory only. Owner must: treat a chat in Auto or Accept edits like an agent without a sandbox.

### Web pages in the owner's browser

Entry points: cross-site requests to Cerberus' address (CSRF), DNS names that resolve to Cerberus (rebinding),
the loopback gateway without a token (`Server.LocalNoAuth`).

Controls:

- State-changing requests authenticated by the cookie must send `X-Cerberus: 1`. A cross-site page cannot add
  that header without a CORS preflight, which Cerberus never approves.
- Cookie-authenticated `/api/*` requests from another site (`Sec-Fetch-Site`) or from a preview page (`Referer`
  on `/preview/`, `Origin: null`, or an `Origin` on one of this host's preview ports) are refused (`fromOwnUI`,
  `fromPreviewOrigin` in [internal/api/auth.go](../internal/api/auth.go)). Two different `cerberus_token`
  cookies on one request (a preview page on another port can plant one with a narrower path) are refused
  rather than guessed between.
- The Host header must be a listen address, the `PublicURL` host, localhost or a `Server.AllowedHosts` entry, on
  every request, or the answer is 403. This defeats DNS rebinding for the UI and the gateway. With a wildcard
  listen address (`0.0.0.0`, `::`) any IP literal is accepted as well: rebinding needs a host name
  (`withHost` in [internal/api/safety.go](../internal/api/safety.go)).
- `LocalNoAuth` on `/v1/*` holds only for a loopback peer with no `Origin`, a local or own Host, and a JSON
  `Content-Type` on writes, so a page's "simple" request cannot use it (`localClient` in
  [internal/api/auth.go](../internal/api/auth.go)).
- The UI sends a strict Content-Security-Policy, `X-Frame-Options: SAMEORIGIN`, `nosniff` and
  `Referrer-Policy: no-referrer` (`withHeaders` in [internal/api/auth.go](../internal/api/auth.go)).

Residual risk: a malicious browser extension sees everything the UI shows. Any local program (not a web page)
can use the loopback gateway when `LocalNoAuth` is on, and spend your remote model budget.

Owner must: turn off `LocalNoAuth` if untrusted local programs run on the host or if remote providers bill you.

### Other hosts on the LAN or tailnet

Entry points: the listen addresses, the gateway, the public paths (`/healthz`, `/login`, `/assets/*`, `/manifest.webmanifest`, `/sw.js`).

Controls:

- Everything but the public paths needs a token. Auth failures are rate-limited and logged.
- Cerberus refuses to start on a public, non-private IP (anything but loopback, RFC 1918, CGNAT 100.64/10,
  ULA fc00::/7 or link-local, also for names that resolve to one) unless `Server.AllowPublic` is set, and warns
  for `0.0.0.0` (`CheckListen` in [internal/api/doctor/doctor.go](../internal/api/doctor/doctor.go)).
- TLS with reloadable certificates ([cmd/cerberus/tls.go](../cmd/cerberus/tls.go)).
- `X-Forwarded-For` is never trusted; only the TCP peer counts.

Residual risk: without TLS a LAN attacker can sniff the token. A device on your tailnet is as good as on your LAN.
The public paths show that Cerberus runs there.

Owner must: use TLS off loopback, keep Cerberus behind your router or tailnet ACLs, and never port-forward it.

### Uploaded files and preview pages

Entry points: file drop into a session's inbox, workspace files served back, `/preview/<id>/…` pages an agent
wrote.

Controls:

- File names are sanitised and paths cleaned ([internal/files/fsafe](../internal/files/fsafe)); every path is
  opened through an `os.Root` on the workspace, so `..` and symlinks cannot escape; size limits apply while
  streaming; deny paths and `.git` are refused; no exec bit unless `Files.AllowExec`
  ([internal/files/files.go](../internal/files/files.go)).
- Workspace files are served with their type from the extension, never sniffed. HTML, SVG and XML are sandboxed by
  CSP, and risky types download as attachments (`rawCSP` in [internal/api/files.go](../internal/api/files.go)).
- Without `server.preview_ports`, preview pages run in an opaque origin (CSP `sandbox`), so their scripts cannot
  read Cerberus or call `/api` with your cookie. Their sub-requests use a per-session capability (an HMAC of the
  server token) that opens only that preview ([internal/api/preview.go](../internal/api/preview.go)).
- With `server.preview_ports`, each open preview (a session's static site, or one of its live servers) is leased
  a port of the range and served at `/` there: a different origin from the app, so the browser keeps it out of
  the app's pages and storage, and the app refuses its requests (above). The app's entry `/preview/<id>/` checks
  the signed-in person and the session's owner, then redirects with `?cerberus_preview=<cap>`: an HMAC of the
  admin token over session, source, port and a random per-lease nonce. The preview server turns it into an
  HttpOnly `SameSite=Lax` cookie `cerberus_pv_<port>` and strips it from the address; nothing else opens the
  port (not the app's cookie or token), the Host header must be one of ours (DNS rebinding), a new lease
  invalidates old links and cookies and sends `Clear-Site-Data: "cache", "storage"`, service worker scripts are
  refused (one would outlive the lease), and `frame-ancestors` allows only the app's origins. Ports bind only
  on the app's hosts, and public addresses are refused as for `listen` ([internal/api/previewsrv.go](../internal/api/previewsrv.go),
  `bindPreview` in [cmd/cerberus/serve.go](../cmd/cerberus/serve.go)).
- Live dev servers: a port is proxied only while a process of the session owns a loopback or wildcard LISTEN
  socket on it (Linux `/proc`: the agent's PTY session and descendants, its systemd scope, the Run preview
  server's session, or a process carrying the session's unguessable `CERBERUS_PREVIEW_TAG`), re-checked at
  most a second before each request, so a session can never point a preview at another local service or the
  LAN. The proxy dials only `127.0.0.1`/`[::1]`, sets `Host`/`Origin` to `localhost:<port>`, drops Cerberus'
  cookies and `X-Cerberus*` headers (and a Cerberus bearer) on the way in and any `Set-Cookie` of Cerberus'
  cookie names on the way out. Other OSes have no detection: only the Run preview server, on the port Cerberus
  picked ([internal/session/preview.go](../internal/session/preview.go)).
- Run preview runs the command from `.cerberus.json` (agent-writable, like checks) or `package.json` in the
  session's sandbox plan (always bwrap for a non-admin owner) and memory scope, without Cerberus' variables or
  the agent key; starting it needs the session's owner or an admin and is audited; it stops on Stop, discard,
  accept, delete, shutdown and after `sessions.preview_idle_min` without visits.

Residual risk: a file you download and open on your own machine runs with your rights there. A preview can still
show a convincing fake page or link out. Rotating the admin token changes preview capabilities. Cookies are
per host, not per port: a preview page can set (not read) cookies that the app and other previews on the same
host receive, which can at worst sign you out (refused as conflicting) or confuse another preview; previews of
different people's sessions share the host. The capability in a preview's first URL is visible to whoever sees
that URL before the redirect; it opens only that one lease.

Owner must: open downloaded files from agents with the care you give any download.

## Audit log

`DATA/audit.jsonl`, append-only, mode 0600, rotated at 8 MiB (5 kept). Each record has the time, actor
(`owner:<device>`, `agent:<session>`, `cli` or `system`), remote address, method, route pattern (never the query),
target ids, status and a short detail. Bodies are never stored; details are masked through vault values and
clipped to 80 characters. Auth failures, rotations, pairing, revocation, sandbox fallbacks and kill or renice of
host processes are logged too. See [internal/audit/audit.go](../internal/audit/audit.go).

Gateway calls under `/v1` are not audited (usage already records them); hidden terminal input (echo off) is
logged only as its length. Read it with `cerberus audit [--since 24h] [--actor agent] [--json]`,
`GET /api/audit?since=&actor=&limit=` or the Security view. An actor kind (`agent`) matches every
`agent:<id>`; a full actor matches exactly.

The log is for noticing, not for proof: anything running as your user can edit or delete it.

## Doctor

`cerberus doctor [--json]`, `GET /api/doctor`, the Security view and a card on Monitor run 16 checks, each ok,
warn or fail with a one-line explanation and a fix: config, file modes, listen addresses, TLS, token age,
LocalNoAuth, sandbox (bwrap), RAPL, engines, agents, systemd-run, disk, retention, git, voice and the service
unit ([internal/api/doctor/doctor.go](../internal/api/doctor/doctor.go)). The CLI exits non-zero on a failure
and never creates a config.

## What Cerberus does not protect against

- **An agent with shell access and no sandbox.** It can read, change and send anything your user can: SSH keys,
  browser cookies, other apps' tokens, the Cerberus config and vault key.
- **A local attacker with your account.** The file vault backend encrypts values, but the key (`vault.key`) sits
  next to them. It guards against backups, grep and screenshots, not against someone who can read your files.
  The keyring backend is better, but an unlocked desktop session hands values to any program you run.
- **Root or a compromised kernel.** Nothing in user space helps there.
- **The public internet.** Cerberus is not built or tested to face it, even with `AllowPublic` and TLS.
- **Prompt injection.** Text an agent reads can steer it. Review diffs, keep pushes asked, sandbox agents.
- **Remote model providers.** What you send them is theirs to keep under their terms. Use local models for
  material that must stay on the host.
- **Spend on a leaked provider key.** Revoke the key at the provider; Cerberus only tracks what passes through it.
- **Your own approvals.** "Approve always", auto mode and accepting with push do what they say.
- **Network attackers when TLS is off.** Tokens travel in clear on plain HTTP.
- **Supply chain of agent CLIs.** Third-party agent CLIs run with your rights and talk to their vendors.

## Reporting a problem

Open an issue on the GitHub project, or for something sensitive, write to the owner privately first.
