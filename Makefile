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
MODULE   := $(shell $(GO) list -m)
COVER    ?= $(CURDIR)/.cache/cover
# The packages tested against the real openh264 and libopus.
CGO_TESTS := ./internal/agent/h264/ ./internal/agent/opus/ ./internal/agent/audio/ ./internal/agent/ ./internal/agent/vqtest/

target_amd64 := x86_64-linux-musl
target_arm64 := aarch64-linux-musl

export ZIG_GLOBAL_CACHE_DIR := $(CACHE)/zig

# zig_env starts a recipe line whose command runs in the agent's cgo
# toolchain for linux/$(1): zig as the C compiler, openh264 and libopus.
define zig_env
t=$(target_$(1)); zig=$$(toolchain/zig.sh); prefix=$$(toolchain/openh264.sh $$t); toolchain/opus.sh $$t >/dev/null; \
CGO_ENABLED=1 CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
CGO_CFLAGS="-I$$prefix/include" CGO_LDFLAGS="-L$$prefix/lib"
endef

# agent_build builds the agent for linux/$(1) into $@, with the extra go
# build flags $(2).
define agent_build
@set -e; echo "build agent linux/$(1) ($(target_$(1)))"; \
$(call zig_env,$(1)) GOOS=linux GOARCH=$(1) \
$(GO) build $(2) -trimpath -tags openh264,opus,netgo,osusergo \
	-ldflags "-s -w -linkmode external -extldflags -static -X main.version=$(VERSION)" \
	-o $@ ./cmd/robin-agent
endef

.PHONY: all agents cli dist test test-js test-short test-x test-wayland check e2e clean
.PHONY: cover cover-unit cover-x cover-wayland cover-e2e cover-report cover-baseline
.PRECIOUS: bin/robin-agent-linux-% bin/cover/robin-agent-linux-%

all: cli

cli: bin/robin

agents: $(foreach a,$(ARCHES),$(AGENTS)/robin-agent-linux-$(a).gz)

bin/robin: agents $(GOSRC) go.mod go.sum
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/robin

bin/robin-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh
	$(call agent_build,$*)

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
	@set -e; $(call zig_env,amd64) \
	$(GO) test -tags openh264,opus $(CGO_TESTS)
endif

# The page's pure modules, under Node's own test runner. make test runs them
# too, through TestPageModules in internal/web; this target is for make cover.
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

# Coverage of every suite, merged in $(COVER): unit, X, Wayland, and e2e
# with the CLI and the agent built with -cover and the page measured in
# Chrome. A failing suite still hands in what it covered; make cover fails
# once the report is out. It compares each package with
# coverage-baseline.txt, which make cover-baseline rewrites from the run.
cover:
	@rm -rf $(COVER)
	@failed=; for s in unit x wayland e2e report; do \
		$(MAKE) --no-print-directory cover-$$s || failed="$$failed $$s"; \
	done; \
	[ -z "$$failed" ] || { echo "make cover: failed:$$failed" >&2; exit 1; }

cover-unit:
	@mkdir -p $(COVER)/profiles $(COVER)/page/unit
	$(GO) test -coverpkg=./... -coverprofile=$(COVER)/profiles/unit.out ./...
ifeq ($(HOST_OS)/$(HOST_ARCH),linux/amd64)
	@set -e; $(call zig_env,amd64) \
	$(GO) test -tags openh264,opus -coverpkg=./... -coverprofile=$(COVER)/profiles/unit-cgo.out $(CGO_TESTS)
endif
	@if command -v node >/dev/null; then NODE_V8_COVERAGE=$(COVER)/page/unit $(MAKE) --no-print-directory test-js; \
	else echo "make cover-unit: no node, no page unit coverage"; fi

cover-x:
	XTEST_COVER=$(COVER)/profiles/x internal/agent/x11/testdata/xtest.sh

cover-wayland:
	WLTEST_COVER=$(COVER)/profiles/wayland internal/agent/wayland/testdata/wltest.sh

cover-e2e: bin/cover/robin-linux-amd64
	ROBIN_BIN=$(CURDIR)/$< ROBIN_E2E_COVER=$(COVER)/e2e e2e/run.sh $(SCENARIOS)

# One shell, so a step that fails (stale page coverage, say) still lets the
# Go summary and the baseline check print before the target fails.
cover-report:
	@mkdir -p $(COVER)/profiles; status=0; \
	dirs=$$(find $(COVER)/e2e -name 'covmeta.*' -exec dirname {} + 2>/dev/null | sort -u | paste -sd, -); \
	if [ -n "$$dirs" ]; then $(GO) tool covdata textfmt -i="$$dirs" -o $(COVER)/profiles/e2e.out || status=$$?; \
	else echo "make cover-report: no e2e coverage data"; fi; \
	pages=$$(find $(COVER) -path '*/page/*' -name '*.json' 2>/dev/null); \
	if [ -z "$$pages" ]; then echo "make cover-report: no page coverage"; \
	elif $(GO) run ./internal/tools/pagecover $$pages >$(COVER)/page.txt; then cat $(COVER)/page.txt; echo; \
	else status=$$?; fi; \
	$(GO) run ./internal/tools/covercheck -module $(MODULE) -baseline coverage-baseline.txt \
		-o $(COVER)/merged.out $(COVER)/profiles >$(COVER)/summary.txt || status=$$?; \
	cat $(COVER)/summary.txt; \
	[ ! -f $(COVER)/merged.out ] || $(GO) tool cover -html=$(COVER)/merged.out -o $(COVER)/cover.html || status=$$?; \
	exit $$status

cover-baseline:
	$(GO) run ./internal/tools/covercheck -module $(MODULE) -baseline coverage-baseline.txt -write $(COVER)/merged.out

# make cover's e2e builds: the agent with -cover, and a CLI with -cover that
# embeds it. The overlay swaps the embedded agent without touching
# $(AGENTS), so no normal build ever ships a cover agent.
bin/cover/robin-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh
	$(call agent_build,$*,-cover)

bin/cover/agents/robin-agent-linux-%.gz: bin/cover/robin-agent-linux-%
	@mkdir -p $(@D)
	gzip -9 -n -c $< >$@

bin/cover/robin-linux-amd64: bin/cover/agents/robin-agent-linux-amd64.gz $(GOSRC) go.mod go.sum
	printf '{"Replace":{"%s":"%s"}}\n' $(CURDIR)/$(AGENTS)/robin-agent-linux-amd64.gz $(CURDIR)/$< >bin/cover/overlay.json
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -cover -overlay bin/cover/overlay.json -trimpath \
		-ldflags "-s -w -X main.version=$(VERSION)" -o $@ ./cmd/robin

clean:
	rm -rf bin dist $(AGENTS)/*.gz
