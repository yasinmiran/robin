# One command builds everything: `make`.
#
# The CLI (bin/robin) is pure Go and embeds the host agents. Each agent
# is a fully static linux binary built with cgo against openh264 and libopus,
# with a pinned zig as the C toolchain, so this works from Linux or macOS with
# no containers and no root.

GO       ?= go
VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
ARCHES   ?= amd64 arm64
HOST_OS  := $(shell $(GO) env GOOS)
HOST_ARCH:= $(shell $(GO) env GOARCH)
CACHE    := $(CURDIR)/.cache/toolchain
AGENTS   := internal/agentbin/agents
GOSRC    := $(shell find cmd internal -type f -not -path "*/node_modules/*" -not -name "*.gz")

target_amd64 := x86_64-linux-musl
target_arm64 := aarch64-linux-musl

export ZIG_GLOBAL_CACHE_DIR := $(CACHE)/zig

.PHONY: all agents cli dist test test-js test-short test-x test-wayland check e2e clean
.PRECIOUS: bin/robin-agent-linux-%

all: cli

cli: bin/robin

agents: $(foreach a,$(ARCHES),$(AGENTS)/robin-agent-linux-$(a).gz)

bin/robin: agents $(GOSRC) go.mod go.sum
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/robin

bin/robin-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh
	@set -e; \
	t=$(target_$*); zig=$$(toolchain/zig.sh); prefix=$$(toolchain/openh264.sh $$t); toolchain/opus.sh $$t >/dev/null; \
	echo "build agent linux/$* ($$t)"; \
	CGO_ENABLED=1 GOOS=linux GOARCH=$* CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
	CGO_CFLAGS="-I$$prefix/include" CGO_LDFLAGS="-L$$prefix/lib" \
	$(GO) build -trimpath -tags openh264,opus,netgo,osusergo \
		-ldflags "-s -w -linkmode external -extldflags -static -X main.version=$(VERSION)" \
		-o $@ ./cmd/robin-agent

$(AGENTS)/robin-agent-linux-%.gz: bin/robin-agent-linux-%
	gzip -9 -n -c $< >$@

# Release CLIs for every client platform, each embedding every agent.
dist: agents
	@set -e; for p in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "build dist/robin-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" -o dist/robin-$$os-$$arch ./cmd/robin; \
	done

# The linux/amd64 CLI the e2e client container runs.
bin/robin-linux-amd64: agents $(GOSRC) go.mod go.sum
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/robin

# Unit and integration tests. The encoder packages are tested against the real
# openh264 and libopus on linux/amd64 hosts; everywhere else they build as stubs.
test:
	$(GO) test ./...
ifeq ($(HOST_OS)/$(HOST_ARCH),linux/amd64)
	@set -e; t=$(target_amd64); zig=$$(toolchain/zig.sh); prefix=$$(toolchain/openh264.sh $$t); toolchain/opus.sh $$t >/dev/null; \
	CGO_ENABLED=1 CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
	CGO_CFLAGS="-I$$prefix/include" CGO_LDFLAGS="-L$$prefix/lib" \
	$(GO) test -tags openh264,opus ./internal/agent/h264/ ./internal/agent/opus/ ./internal/agent/audio/
endif
	@if command -v node >/dev/null; then $(MAKE) --no-print-directory test-js; else echo "make test: no node, skipping test-js"; fi

# The page's pure modules, under Node's own test runner.
test-js:
	node --test internal/web/static/test/

test-short:
	$(GO) test -short ./...

# The capture, input, display and agent session tests, against a real Xvfb in
# a podman container.
test-x:
	internal/agent/x11/testdata/xtest.sh

# Wayland attach against headless GNOME 46, GNOME 49 and KDE Plasma 6.7, in
# podman: consent, restore, frames, input, clipboard, teardown and PipeWire
# sound capture. KDE needs a GPU render node and is skipped without one.
test-wayland:
	internal/agent/wayland/testdata/wltest.sh

# Everything: unit, X, Wayland and end-to-end.
check: test test-x test-wayland e2e

e2e: bin/robin-linux-amd64
	ROBIN_BIN=$(CURDIR)/bin/robin-linux-amd64 e2e/run.sh $(SCENARIOS)

clean:
	rm -rf bin dist $(AGENTS)/*.gz
