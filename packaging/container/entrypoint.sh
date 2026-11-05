#!/bin/bash
# Container entrypoint: write the config on the first start, apply the
# settings that come from the environment, create the first admin account
# when asked, then run the command (default: cerberus serve).
#
# Environment (all optional):
#   CERBERUS_LISTEN            address inside the container (default 0.0.0.0:7340)
#   CERBERUS_PUBLIC_URL        server.public_url, the address people use
#   CERBERUS_OLLAMA_URL        Ollama to use, e.g. http://host.containers.internal:11434
#   CERBERUS_ADMIN_USER        name of the first admin account (only when none exists)
#   CERBERUS_ADMIN_PASSWORD_FILE  file holding its password
#                              (default /run/secrets/cerberus_admin_password)
set -euo pipefail

cfg="$XDG_CONFIG_HOME/cerberus/config.json"
mkdir -p "$XDG_CONFIG_HOME" "$XDG_DATA_HOME" "$XDG_STATE_HOME"

# Anything other than "serve" (cerberus user list, cerberus url, bash…) runs as is.
if [ "${1:-serve}" != "serve" ]; then
  case "$1" in
    cerberus|bash|sh|/*) exec "$@" ;;
    *) exec cerberus "$@" ;;
  esac
fi

set_cfg() { cerberus config set "$1" "$2" >/dev/null; }

if [ ! -f "$cfg" ]; then
  echo "cerberus: first start, writing $cfg"
  cerberus init --yes >/dev/null
  # Defaults that fit a container. Changed later from the UI or with
  # "podman exec cerberus cerberus config set …", they stay changed.
  set_cfg engines.lmstudio.enabled false   # LM Studio is a desktop app on the host
  set_cfg engines.llamacpp.enabled false   # no llama-server in the image
  set_cfg guard.systemd_slice ""            # no systemd user bus in a container
  set_cfg server.local_no_auth false        # every caller shows a key or a sign-in
  set_cfg repo_roots '["/repos"]'           # mount repositories at /repos
  set_cfg sandbox.default bwrap
  set_cfg server.preview_ports 7350-7369    # work previews; published like 7340
fi

# Set on every start, so the container's environment is the authority.
set_cfg server.listen "${CERBERUS_LISTEN:-0.0.0.0:7340}"
[ -n "${CERBERUS_PUBLIC_URL:-}" ] && set_cfg server.public_url "$CERBERUS_PUBLIC_URL"
[ -n "${CERBERUS_OLLAMA_URL:-}" ] && set_cfg engines.ollama.url "$CERBERUS_OLLAMA_URL"

# First admin account, only while there is none.
pwfile="${CERBERUS_ADMIN_PASSWORD_FILE:-/run/secrets/cerberus_admin_password}"
if [ -n "${CERBERUS_ADMIN_USER:-}" ]; then
  existing="$(cerberus user list --json 2>/dev/null | tr -d ' \n')"
  if [ "$existing" != "[]" ] && [ "$existing" != "null" ]; then
    : # accounts exist: never touch them from here
  elif [ -r "$pwfile" ]; then
    tr -d '\r\n' < "$pwfile" | cerberus user add "$CERBERUS_ADMIN_USER" --role admin --password-stdin
    echo "cerberus: created admin account $CERBERUS_ADMIN_USER"
  else
    echo "cerberus: CERBERUS_ADMIN_USER is set but $pwfile is missing; no account created" >&2
  fi
fi

exec cerberus "$@"
