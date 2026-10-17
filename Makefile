# One command builds everything: `make`.
#
# The CLI (bin/remotejump) is pure Go and embeds the host agents. Each agent
# is a fully static linux binary built with cgo against openh264, with a
# pinned zig as the C toolchain, so this works from Linux or macOS with no
# containers and no root.

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

.PHONY: all agents cli dist test test-short test-x check e2e clean
.PRECIOUS: bin/remotejump-agent-linux-%

all: cli

cli: bin/remotejump

agents: $(foreach a,$(ARCHES),$(AGENTS)/remotejump-agent-linux-$(a).gz)

bin/remotejump: agents $(GOSRC) go.mod go.sum
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/remotejump

bin/remotejump-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh
	@set -e; \
	t=$(target_$*); zig=$$(toolchain/zig.sh); prefix=$$(toolchain/openh264.sh $$t); toolchain/opus.sh $$t >/dev/null; \
	echo "build agent linux/$* ($$t)"; \
	CGO_ENABLED=1 GOOS=linux GOARCH=$* CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
	CGO_CFLAGS="-I$$prefix/include" CGO_LDFLAGS="-L$$prefix/lib" \
	$(GO) build -trimpath -tags openh264,netgo,osusergo \
		-ldflags "-s -w -linkmode external -extldflags -static -X main.version=$(VERSION)" \
		-o $@ ./cmd/remotejump-agent

$(AGENTS)/remotejump-agent-linux-%.gz: bin/remotejump-agent-linux-%
	gzip -9 -n -c $< >$@

# Release CLIs for every client platform, each embedding every agent.
dist: agents
	@set -e; for p in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do \
		os=$${p%/*}; arch=$${p#*/}; \
		echo "build dist/remotejump-$$os-$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath \
			-ldflags "-s -w -X main.version=$(VERSION)" -o dist/remotejump-$$os-$$arch ./cmd/remotejump; \
	done

# The linux/amd64 CLI the e2e client container runs.
bin/remotejump-linux-amd64: agents $(GOSRC) go.mod go.sum
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/remotejump

# Unit and integration tests. The encoder package is tested against the real
# openh264 on linux/amd64 hosts; everywhere else it builds as a stub.
test:
	$(GO) test ./...
ifeq ($(HOST_OS)/$(HOST_ARCH),linux/amd64)
	@set -e; t=$(target_amd64); zig=$$(toolchain/zig.sh); prefix=$$(toolchain/openh264.sh $$t); \
	CGO_ENABLED=1 CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
	CGO_CFLAGS="-I$$prefix/include" CGO_LDFLAGS="-L$$prefix/lib" \
	$(GO) test -tags openh264 ./internal/agent/h264/
endif

test-short:
	$(GO) test -short ./...

# The capture, input, display and agent session tests, against a real Xvfb in
# a podman container.
test-x:
	internal/agent/x11/testdata/xtest.sh

# Everything: unit, X and end-to-end.
check: test test-x e2e

e2e: bin/remotejump-linux-amd64
	RJ_BIN=$(CURDIR)/bin/remotejump-linux-amd64 e2e/run.sh $(SCENARIOS)

clean:
	rm -rf bin dist $(AGENTS)/*.gz
