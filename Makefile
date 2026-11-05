# Cerberus build. Pure Go (CGO_ENABLED=0), so every target cross-compiles.
# Pre-release versions: 0.0.<number of commits>. No release tags yet.
# The build is the short commit, with "-dirty" for uncommitted changes.
VERSION := 0.0.$(shell git rev-list --count HEAD 2>/dev/null || echo 0)
BUILD := $(shell git describe --always --dirty 2>/dev/null)
LDFLAGS := -s -w -X main.version=$(VERSION) -X main.build=$(BUILD)
BUILDFLAGS := -trimpath -ldflags "$(LDFLAGS)"
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64

export CGO_ENABLED := 0

.PHONY: build test test-web vet cross clean container container-run

build:
	go build $(BUILDFLAGS) -o dist/cerberus ./cmd/cerberus

test:
	CGO_ENABLED=1 go test -race ./...  # the race detector needs cgo; the binary does not

# Node 22 does not expand a directory argument, so pass the files.
test-web:
	node --test web/test/*.test.mjs
	@for f in $$(find web -name '*.js' -not -path '*/vendor/*'); do node --check $$f || exit 1; done

vet:
	go vet ./...
	GOOS=darwin go vet ./...
	GOOS=windows go vet ./...

cross:
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		echo "build $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch go build $(BUILDFLAGS) -o dist/cerberus-$$os-$$arch$$ext ./cmd/cerberus || exit 1; \
	done

clean:
	rm -rf dist

# Container image (podman first; CONTAINER=docker works too). docker format
# keeps the HEALTHCHECK. See docs/CONTAINER.md.
CONTAINER ?= podman
IMAGE ?= localhost/cerberus:latest
FORMAT := $(if $(filter podman buildah,$(notdir $(CONTAINER))),--format docker,)

container:
	$(CONTAINER) build $(FORMAT) --build-arg VERSION=$(VERSION) --build-arg BUILD=$(BUILD) -t $(IMAGE) .

# A quick trial on 127.0.0.1:7340 with a named volume; stop with
# "$(CONTAINER) rm -f cerberus".
container-run:
	$(CONTAINER) run -d --name cerberus -p 127.0.0.1:7340:7340 -p 127.0.0.1:7350-7369:7350-7369 -v cerberus-data:/var/lib/cerberus \
		--security-opt label=disable --security-opt 'unmask=/proc/*' $(IMAGE)
