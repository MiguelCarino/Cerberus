# Users and roles

This guide is for the person who runs Cerberus for several people. The contract behind it is
"Users and roles (Phase 4a)" in [SCOPE.md](SCOPE.md); the threat model is in [SECURITY.md](SECURITY.md).

## 1. What multi-user Cerberus is, and what it is not

Cerberus can have several accounts. Each person signs in with a user name and password and gets a role.
People with role `user` see only their own sessions, chats, flows, uploads and notifications. Admins see
everything.

It is still one process run as one operating-system user. Other people's agents are kept in by bubblewrap
(bwrap) and by path checks, not by separate OS accounts. Treat it as a shared workshop for people you know
(family, a small team), not as a hosting service for strangers. An admin can do anything the OS user can do.

With no accounts, nothing changes: the admin token and paired devices work as before, and everyone is
the admin. Multi-user mode starts when you add the first account.

## 2. Run it as a service

```
cerberus install-service          # ~/.local/bin/cerberus and a systemd user unit, enabled and started
loginctl enable-linger $USER      # keep it running after you log out
cerberus doctor                   # check the setup
```

Without lingering, the user service stops when the OS user logs out. `cerberus doctor` warns about it
(check `linger`).

Or run it in a container with podman (Quadlet) or Docker: see [CONTAINER.md](CONTAINER.md). The
entrypoint can create the first admin from a podman secret.

## 3. Listen on the LAN or tailnet

By default Cerberus listens on `127.0.0.1:7340` only. To let other people in:

- Add an address to `server.extra_listen`, for example your tailnet address `100.x.y.z:7340`.
- Use HTTPS on it. Passwords must not cross the network in clear. Get a certificate with
  `tailscale cert <name>` and set `server.tls_cert` and `server.tls_key`; extra listen addresses then
  serve HTTPS. Or put Tailscale serve in front. `cerberus doctor` fails `users_tls` when accounts exist
  and a non-loopback address serves plain HTTP.
- Add the names people type to `server.allowed_hosts` (DNS rebinding guard), and set `server.public_url`
  so links in notifications work.
- Never put Cerberus on the public internet (`server.allow_public` stays off).

## 4. The first admin

Either on the host:

```
cerberus user add ana --role admin          # asks for the password twice, no echo
```

or in the browser: sign in with the admin token (`cerberus url` prints a link, or use "Sign in with the
admin token" on the login page). With no accounts the app opens the first-run screen, which creates the
admin and signs you in as them. The first account must be an admin; the CLI exits 2 otherwise.

All data from before accounts existed (sessions, chats, flows, runs, inbox items, uploads, devices and
MCP clients) now belongs to this first admin.

## 5. Adding users

- CLI: `cerberus user add NAME --role ROLE [--display TEXT] [--password-stdin | --generate]`.
  `--generate` prints a 16-character password once.
- Web: Settings → Users → Add user. Type a password or let it generate one; a generated password is shown
  once with a Copy button.

A generated or admin-reset password must be changed at the first sign-in: until then the person can only
change it (and sign out). Names are lowercase `a-z 0-9 . _ -`, 1 to 32 characters. Passwords are 10 to
256 bytes and not the user name; nothing else is required.

## 6. Roles

| Role | Can |
|---|---|
| admin | Everything: every user's sessions and chats (read, delete, stop), Settings, the vault, the audit log, Users, models (load, download, delete), fine-tuning, devices, token rotation. |
| user | Own chats (with file tools in their own folder), own agent sessions and flows (sandboxed), uploads, the models list, the /v1 gateway with a personal API key, voice, own devices. Within their budget. |
| viewer | Read only: every user's sessions (list, wall, terminals, transcripts, diffs, files, replays, work previews) and the Monitor page with doctor counts. Cannot type, approve, start, stop or delete, nor start or stop a preview server. No chats. |
| monitor | The Monitor page only (power, energy, bill, processes read-only, doctor counts). |
| chat | Own LLM chats only, as plain chat with images: no tools that read or write files or run commands. |

Notes:

- Chats are private: an admin can list, read, delete and stop any chat but cannot write into someone
  else's chat (it would act with their tools and budget).
- `user` does not get the Monitor page: it shows host-wide processes and the household bill. Give
  someone `viewer` for that.
- The UI hides what a role cannot use; the server checks the role and the owner on every request.
- Work previews (a session's Preview tab) follow the session: its owner, admins and viewers can
  watch them; only the owner or an admin can **Run preview** or stop it (audited). A preview link
  copied from the tab is the app's own address: whoever opens it signs in, and someone who may not
  see the session gets "not found". The preview's own origin takes only the one-time capability
  the app hands out on entry, never your sign-in.

## 7. Sharing repos and agent profiles

A `user` starts sessions in a scratch folder, or in a repo an admin shared with them
(`cerberus user share NAME /path/to/repo [--accept]`, or Settings → Users → Shared repos). `--accept` lets
them fast-forward the repo's main; without it they can diff, save (under their own folder) and download a
zip, and an admin accepts.

Folder workspaces (editing any folder in place) are admin only: a folder can be anywhere in the OS
user's home.

Sessions of non-admin users always run in bwrap, whatever `sandbox.default` says. If bwrap is missing the
start is refused (503 "sandbox"); there is never a fallback. In the sandbox the whole home folder is
replaced by an empty folder: other repos, `~/.config/gh`, `~/.aws`, `~/.netrc`, shell history and the like
are not there. Bound back are only the session's workspace and session folder, a shared repo's git folder
(read only), the agent's program when it lives under the home (read only), `sandbox.extra_ro` (read only:
add toolchains under the home here, for example `~/go` or `~/.cargo`, and `~/.gitconfig` if agents should commit) and the agent profile's own
`sandbox_rw`. The global `sandbox.extra_rw` binds are dropped, and Cerberus' data folder, config folder
and `$XDG_RUNTIME_DIR` are hidden, so `users.json`, other people's sessions and chats are out of reach.
Confinement is recomputed from the owner's current role at every run, and the fields that decide it are
kept outside the session's own (writable) folder.

What stays visible: the rest of the filesystem read-only (`/usr`, `/etc`, `/opt`, other OS users' homes
as far as their modes allow) and, with `sandbox.network` on (the default, needed to reach the gateway),
the network: the LAN, the internet and every port on this machine's loopback. Keep secrets out of
world-readable places, and see section 8 for `local_no_auth`.
`cerberus doctor` fails `users_sandbox` when a `user` account exists and bwrap is not usable.

By default a user may use only the built-in agent (`cerberus`). An admin can allow other profiles per
user. Sharing a CLI profile (claude, opencode) shares your own login of that tool, because its config
folder is bound into the sandbox: do that only for people you would lend the login to.

Chats of role `user` work in `DATA/users/<id>/files/`: file tools refuse anything outside it, and
commands run in bwrap with only that folder writable (no bwrap: no command tool). The `claude` chat
backend and the Atrium / Claude Code imports are admin only.

## 8. Budgets and personal API keys

- Each account can have a monthly cap in US dollars and/or tokens (`cerberus user budget NAME --usd 5`,
  Settings → Users; 0 = none). The month is the calendar month in local time.
- The cap is checked when a session starts, a chat message is sent, a quick answer or flow runs, and on
  every `/v1` request. Over budget the API answers 429 (`code: budget`) and `/v1` an OpenAI-style 429
  (`type: budget_exceeded`). At 80 % the user gets one inbox item per month.
- Spend is attributed per request: a personal key, cookie or device counts for its user; a session's
  agent for the session's owner; chats and flow runs for their owner.
- Not counted: tokens of subscription CLIs (claude, opencode) that talk to their vendor directly. Decide
  whether to share such a profile with that in mind.
- Personal `/v1` keys (`cuk-…`, Account → API key) are for admins and users. A personal key counts
  for its user also from this machine.
- `server.local_no_auth` lets local programs use `/v1` without a key, as the admin, outside every
  budget. Confined sessions and user chats share the host network, so while one of them runs, keyless
  local requests are refused (401 `local_no_auth_paused`; send a key). Anything else a user can start on
  this machine (an OS login, a cron job) still gets in as the admin: with `user` accounts, turn it off
  (`cerberus doctor` fails `local_no_auth`) and give people personal keys. Budgets are not enforced
  while it is on.
- Global per-provider budgets (`budgets`) still apply on top.

## 9. Devices and MCP clients per user

- A signed-in person pairs their own phone or laptop (Account → Devices); the admin token can pair a
  device for anyone (`user` in `POST /api/devices`). A device acts with its user's role.
- MCP clients (`cerberus pair --mcp`) need role admin or user. Sessions they start belong to that user,
  sandboxed and budgeted like the user's own.
- Admins see and revoke every device; others only their own.

## 10. Sign-in, lockout, sign out, password changes

- A browser sign-in lasts at most `users.session_max_days` (30) and ends after `users.session_idle_hours`
  (336, two weeks) without use. Account → Signed-in browsers lists them; sign out any.
- Five wrong passwords for one name within 15 minutes lock that name for 15 minutes
  (`users.lockout_failures`, `users.lockout_minutes`); unknown names lock the same way. Each address is
  also limited to 5 bad credentials a minute (then 10 minutes of 429); attempts still being checked
  count, so a burst of parallel guesses gets no further. Password sign-ins from loopback are not limited
  per address, only per name: behind `tailscale serve` or another local proxy every sign-in comes from
  127.0.0.1, and one person's typos must not lock everyone out. An admin password reset clears a
  lockout.
- Changing your own password (Account) signs out every other browser and revokes your devices and MCP
  clients.
- Disabling a user, changing their role, resetting their password or deleting them ends all their
  sign-ins, revokes their devices and MCP clients, and cuts their live streams at once. A role change or
  password reset leaves their running sessions alone; disabling or deleting them stops their running
  sessions (the reply says how many).
- Forgotten password: `cerberus user passwd NAME` (or Settings → Users → Reset password). It must be
  changed at the next sign-in.

## 11. The admin token

The admin token stays the break-glass and bootstrap credential: `cerberus url`, `?token=` links, "Sign in
with the admin token", `cerberus pair` and the CLI keep working. It acts as the first admin and is
audited as `owner:admin`. Rotate it after any doubt (`cerberus token rotate`). Keep it for yourself;
give everyone else an account.

## 12. Backups

Back these up together, with the service stopped (`systemctl --user stop cerberus`) or with care:

- `DATA/users.json` (accounts; 0600) and, optionally, `DATA/logins.json` (browser sign-ins; without it
  everyone signs in again). Once an account exists, `DATA/users.enabled` marks it: without `users.json`
  the daemon then refuses to start (and a running one keeps its last list) instead of falling back to
  single-user mode, where every paired device and agent acts as the admin. Restore `users.json` from the
  backup; delete `users.enabled` only to go back to single-user mode on purpose (devices and confined
  sessions bound to an account then stop working);
- `config/devices.json` and `config.json` (the admin token);
- `DATA/sessions/`, `DATA/chats/`, `DATA/flows/`, `DATA/users/` (users' own folders);
- the vault (the desktop keyring, or the vault files next to the config).

`DATA` is `~/.local/share/cerberus` and the config folder `~/.config/cerberus` by default. Restore with the
same modes (0600 files, 0700 folders); `cerberus doctor` checks the sensitive ones.

## 12a. Saved permissions

Each person has their own saved permissions ("Always allow" in a chat, "Always ▾ → Every time" on an
agent session's approval in the Sessions list or the bell, or added by hand). They answer only that
person's chats and sessions; nobody else's, not even an admin's, sees or uses them. They live in
`DATA/permits.json` (0600). Those saved before users existed become the first admin's; a removed user's
rules are not handed on (a yes is personal) and simply stop applying. A user opens them from
Account → Saved permissions (Settings → Permissions), an admin from Settings → Permissions; Remove makes
the next matching call ask again. The command policy (`agent.policy`) still wins: a forbidden command is
never run and an "always ask" one always asks. The chat-only role has no tools, so it saves none.

## 13. Removing a user

`cerberus user rm NAME [--to ADMIN] [--yes]` or Settings → Users → Delete. Nothing is deleted: the
user's sessions, chats, flows, runs and inbox items move to the admin who deletes (or `--to`), lazily,
through `moved` in `users.json`. Their devices, MCP clients, API key and sign-ins are revoked, and their
running sessions stop. Their flows move too but do not run (neither on schedule nor by hand) until an
admin opens and saves each one, which makes it theirs: the steps were written by the removed account.
Scheduled ones are turned off (`flows_paused` in the reply). The last enabled admin can never be
removed, disabled or demoted.

## 14. Audit log per user

Every record carries the acting user's name. Password sign-ins act as `user:<name>`; devices stay
`owner:<device>` with the user set; the admin token is `owner:admin` with no user. Filter with
`cerberus audit --user NAME` (`--user -` for records with no user) or the user filter in Settings →
Security. New events: `auth.login`, `auth.login_failed`, `auth.locked`, `user.add`, `user.role`,
`user.disable`, `user.enable`, `user.password`, `user.remove`, `user.apikey`, `user.revoked`. Passwords
and request bodies are never stored.
