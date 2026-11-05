# Cerberus container image. Podman first; Docker works too.
#
#   podman build --format docker -t localhost/cerberus .   (or: make container)
#   podman run -d --name cerberus -p 127.0.0.1:7340:7340 \
#     -v cerberus-data:/var/lib/cerberus localhost/cerberus
#
# See docs/CONTAINER.md for Quadlet (systemd), compose, models on the
# host, user accounts and the sandbox.

# ---- build: a static Go binary (CGO_ENABLED=0) ----
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY packaging ./packaging
COPY web ./web
ARG VERSION=0.0.0
ARG BUILD=container
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.build=${BUILD}" \
      -o /out/cerberus ./cmd/cerberus

# ---- runtime ----
FROM registry.fedoraproject.org/fedora-minimal:44
# git: worktrees for sessions. bubblewrap: the session sandbox (required for
# non-admin users). procps-ng/findutils/which: what the built-in agent's
# commands usually reach for. curl: the health check.
RUN microdnf -y install --setopt=install_weak_deps=0 \
      git-core bash bubblewrap ca-certificates procps-ng findutils which \
      diffutils tar gzip less curl-minimal shadow-utils \
 && microdnf clean all \
 && useradd --uid 1000 --create-home --home-dir /home/cerberus --shell /bin/bash cerberus \
 && mkdir -p /var/lib/cerberus /repos \
 && chown cerberus:cerberus /var/lib/cerberus /repos

COPY --from=build /out/cerberus /usr/local/bin/cerberus
COPY packaging/container/entrypoint.sh /usr/local/bin/cerberus-entrypoint

# Everything Cerberus keeps (config with the admin token, accounts, sessions,
# chats, vault, audit log) lives under one volume.
ENV HOME=/home/cerberus \
    XDG_CONFIG_HOME=/var/lib/cerberus/config \
    XDG_DATA_HOME=/var/lib/cerberus/data \
    XDG_STATE_HOME=/var/lib/cerberus/state \
    CERBERUS_LISTEN=0.0.0.0:7340 \
    CERBERUS_CONTAINER=1

USER cerberus
WORKDIR /home/cerberus
VOLUME ["/var/lib/cerberus"]
EXPOSE 7340

HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD curl -fsS -o /dev/null http://127.0.0.1:7340/healthz || exit 1

LABEL org.opencontainers.image.title="Cerberus" \
      org.opencontainers.image.description="Self-hosted LLM runtime: models, one OpenAI-compatible gateway and agent sessions you steer from a web UI" \
      org.opencontainers.image.source="https://github.com/MiguelCarino/Cerberus" \
      org.opencontainers.image.url="https://cerberus.carino.systems" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"

ENTRYPOINT ["/usr/local/bin/cerberus-entrypoint"]
CMD ["serve"]
