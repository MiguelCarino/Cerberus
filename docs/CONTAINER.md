# Cerberus in a container

Podman first; Docker works with the same files. The image is the static
`cerberus` binary on Fedora minimal (about 190 MB) with git, bubblewrap and
the tools the built-in agent's commands usually need. It runs as an
unprivileged `cerberus` user (uid 1000 inside the container).

Files:

| File | What it is |
|---|---|
| `Containerfile` | The image: a Go build stage, then the runtime. |
| `packaging/container/entrypoint.sh` | First-start config, settings from the environment, the first admin account. |
| `packaging/container/cerberus.container` | Quadlet unit: systemd runs the container (recommended). |
| `packaging/container/cerberus.network` | Quadlet network shared with Ollama. |
| `packaging/container/cerberus-ollama.container` | Optional Quadlet unit: Ollama next to Cerberus, with the GPU. |
| `packaging/container/compose.yaml` | The same pair for `podman compose` / `docker compose`. |

## Build

```
make container                 # podman build --format docker -t localhost/cerberus:latest .
make container CONTAINER=docker
```

`--format docker` keeps the image's HEALTHCHECK (the OCI format drops it).

## Quick trial

```
make container-run             # 127.0.0.1:7340, volume cerberus-data
podman exec cerberus cerberus url      # sign-in link with the admin token
podman logs -f cerberus
podman rm -f cerberus
```

## As a service (Quadlet)

Rootless, for your user:

```
mkdir -p ~/.config/containers/systemd
cp packaging/container/cerberus.container packaging/container/cerberus.network \
   packaging/container/cerberus-ollama.container ~/.config/containers/systemd/
systemctl --user daemon-reload
systemctl --user start cerberus-ollama cerberus
loginctl enable-linger $USER     # keep running after you log out
```

For the whole host (rootful), copy the files to `/etc/containers/systemd/`
and drop `--user`. Edit the unit to change ports, volumes or settings, then
`systemctl --user daemon-reload && systemctl --user restart cerberus`.
`AutoUpdate=local` lets `podman auto-update` restart Cerberus when you rebuild
the image.

Do not run the container and `cerberus install-service` on the same port:
both default to 7340.

## Data

Everything lives in one volume at `/var/lib/cerberus`: `config/` (config.json
with the admin token, devices), `data/` (accounts, sessions, chats, vault,
audit log, usage) and `state/`. Back it up with
`podman volume export cerberus-data > cerberus-backup.tar` (stop the
container first for a consistent copy).

## Settings from the environment

The entrypoint writes a config on the first start (`cerberus init --yes`) with
container defaults: LM Studio and llama.cpp off, no systemd slice,
`server.local_no_auth` off, `repo_roots` = `/repos`, `sandbox.default` =
`bwrap`, `server.preview_ports` = `7350-7369`. Change them later in Settings or with
`podman exec cerberus cerberus config set …`; they stay changed.

These variables are applied on every start:

| Variable | Setting | Default |
|---|---|---|
| `CERBERUS_LISTEN` | `server.listen` inside the container | `0.0.0.0:7340` |
| `CERBERUS_PUBLIC_URL` | `server.public_url` (links in notifications) | unset |
| `CERBERUS_OLLAMA_URL` | `engines.ollama.url` | unset (the config's value) |

## User accounts

Create the first admin when the container first starts:

```
printf '%s' 'a long password' | podman secret create cerberus_admin_password -
```

then set `CERBERUS_ADMIN_USER=admin` and `Secret=cerberus_admin_password` in
the Quadlet unit (both are there, commented). The account is created only
while there are none; the secret is never read again after that.
Without it, sign in with the admin token (`podman exec cerberus cerberus url`)
and create the first admin on the screen that follows. Everything else is the
same as on a host: `podman exec cerberus cerberus user add bob --role user`,
or Settings, then Users. See [USERS.md](USERS.md).

## Models

- **Ollama in its own container (default in the units):**
  `cerberus-ollama.container` joins the private `cerberus` network, publishes
  no port, and Cerberus reaches it as `http://cerberus-ollama:11434`. For an
  NVIDIA GPU, install `nvidia-container-toolkit` and generate the CDI spec once
  (`sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`); without a GPU
  remove `AddDevice=` and it runs on the CPU. Pull models with
  `podman exec cerberus cerberus pull qwen3:8b`.
- **Ollama already on the host:** an Ollama on the host's 127.0.0.1 is not
  reachable from a container by default. `Network=pasta:--map-host-loopback,169.254.1.2`
  with `CERBERUS_OLLAMA_URL=http://host.containers.internal:11434` maps it in.
  That maps every port on the host's loopback, and agents inside the container
  can reach them too, so use it only when nothing on the host's 127.0.0.1
  trusts local callers (for example a host Cerberus with `local_no_auth` on).
- **Cloud providers:** add them in Settings as on a host.

## Repositories

Mount them at `/repos` (`Volume=%h/Github:/repos:z`). With rootless podman the
files are owned by your host user inside the container only if you also add
`UserNS=keep-id:uid=1000,gid=1000`. An admin shares repos per user
(`cerberus user share bob /repos/notes`).

## The session sandbox

Non-admin users' sessions always run in bubblewrap, and refuse to start
without it. Inside a container bubblewrap mounts its own `/proc` and
`/dev/pts`, which needs `SecurityLabelDisable=true` and `Unmask=/proc/*`
(`--security-opt label=disable --security-opt unmask=/proc/*`). The default
seccomp profile stays on and no capability is added. `cerberus doctor`
checks it ("Session sandbox (bwrap)").

## Work previews

A session's site opens in its Preview tab or a new window, each preview on
its own origin: a port of `server.preview_ports` (`7350-7369` in the
container). The units and `make container-run` publish that range on
127.0.0.1 next to 7340, so `http://localhost:7340` opens previews on
`http://localhost:7350` and up. Keep both on the same host and publish the
range (and 7340) on the same port numbers: the preview link uses the host name
the browser used for Cerberus and the port number inside the container. To change the range, change
it in Settings (or `config set server.preview_ports`) and in the published
ports. Behind `tailscale serve` or another proxy that serves only 7340,
previews of dev servers are not reachable from other machines; static
previews still open there if you also clear `server.preview_ports` (they then
use the older `/preview/<id>/` path). `cerberus doctor` checks the range
("Preview ports").

## HTTPS and other machines

The units publish on 127.0.0.1 only. For phones and other machines, put HTTPS
in front: `tailscale serve --bg 7340` on the host, or a reverse proxy such as
Caddy. Do not publish plain HTTP on a shared LAN once user accounts exist:
passwords would cross it in clear. Inside the container the doctor cannot see
the published port, so it says so instead of failing.

## What differs from a host install

- No desktop notifications, LM Studio, llama.cpp or GPU monitoring inside the
  container (Monitor shows the container's view).
- No systemd slice per session: the container's own limits apply
  (`podman run --memory`, or `MemoryMax=` under `[Service]` in the unit).
- Only the built-in agent is installed. To add Claude Code or others, build
  your own image `FROM localhost/cerberus` and install them.
- The doctor knows it runs in a container (`CERBERUS_CONTAINER=1`) and words
  the listen, TLS, service, slice and lingering checks for that.
