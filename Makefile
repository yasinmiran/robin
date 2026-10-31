# One command builds everything: `make`.
#
# The CLI (bin/robin) is pure Go and embeds the host agents. Each agent
# is a fully static linux binary built with cgo against openh264 and libopus,
# with a pinned zig as the C toolchain, so this works from Linux or macOS with
# no containers and no root.

GO       ?= go
# Only release tags name a version, at a fixed abbreviation, so two clones
# of one commit describe it alike whatever else they have tagged.
VERSION  ?= $(shell git describe --tags --match 'v[0-9]*' --abbrev=12 --always --dirty 2>/dev/null || echo dev)
ARCHES   ?= amd64 arm64
HOST_OS  := $(shell $(GO) env GOOS)
HOST_ARCH:= $(shell $(GO) env GOARCH)
CACHE    := $(CURDIR)/.cache/toolchain
AGENTS   := internal/agentbin/agents
GOSRC    := $(shell find cmd internal -type f -not -path "*/node_modules/*" -not -name "*.gz")
MODULE   := $(shell $(GO) list -m)
# Each checkout keeps its own coverage: worktrees share .cache through a
# link, and a run starts by clearing COVER.
COVER    ?= $(CURDIR)/.cover
# The packages tested against the real openh264 and libopus.
CGO_TESTS := ./internal/agent/h264/ ./internal/agent/opus/ ./internal/agent/audio/ ./internal/agent/camera/ ./internal/agent/ ./internal/agent/vqtest/
# Every shipped binary is reproducible: the same commit built from any
# checkout path gives the same bytes. -trimpath keeps the checkout out of
# Go's paths, -buildvcs=false keeps git state out of the build info, and
# the linker writes no build ID, which would hash the absolute cgo flags.
# make repro proves it.
GOBUILD  := $(GO) build -trimpath -buildvcs=false
# Not LDFLAGS: make would hand a variable of that name on to the library
# builds in place of the environment's.
GO_LDFLAGS := -s -w -buildid= -X main.version=$(VERSION)
# VERSION is linked into every binary, so a changed one has to rebuild
# them: the file holds the last one built and is rewritten when it moves.
VERSION_FILE := bin/.version
$(shell mkdir -p bin; [ "$$(cat $(VERSION_FILE) 2>/dev/null)" = "$(VERSION)" ] || printf '%s\n' "$(VERSION)" >$(VERSION_FILE))
# zig cc compiles a file it gets no -O for as a debug build with UBSan on,
# which puts a sanitizer runtime and the absolute source path of every cgo
# file into the agent. -Og keeps zig's stack protector, which every -O
# above it drops.
AGENT_CGO_CFLAGS := -Og -fno-sanitize=undefined

# The compiler is an input to the bytes. go.mod names the one release;
# exporting it here makes go switch to it whatever the builder has
# installed or saved with go env -w, and go-version checks that it did.
# GOENV=off keeps the builder's saved go env out, and the rest pin what
# would otherwise be read from the environment.
export GOTOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.mod)
export GOENV := off
export GOFLAGS := -mod=readonly
export GOEXPERIMENT :=
export GOAMD64 := v1
export GOARM64 := v8.0

target_amd64 := x86_64-linux-musl
target_arm64 := aarch64-linux-musl

export ZIG_GLOBAL_CACHE_DIR := $(CACHE)/zig

# zig_env starts a recipe line whose command runs in the agent's cgo
# toolchain for linux/$(1): zig as the C compiler, openh264 and libopus.
define zig_env
t=$(target_$(1)); zig=$$(toolchain/zig.sh); h264=$$(toolchain/openh264.sh $$t); opus=$$(toolchain/opus.sh $$t); \
CGO_ENABLED=1 CC="$$zig cc -target $$t" CXX="$$zig c++ -target $$t" \
CGO_CFLAGS="$(AGENT_CGO_CFLAGS) -I$$h264/include -I$$opus/include" CGO_CPPFLAGS= CGO_CXXFLAGS= \
CGO_LDFLAGS="-L$$h264/lib -L$$opus/lib"
endef

# agent_build builds the agent for linux/$(1) into $@, with the extra go
# build flags $(2).
define agent_build
@set -e; echo "build agent linux/$(1) ($(target_$(1)))"; \
$(call zig_env,$(1)) GOOS=linux GOARCH=$(1) \
$(GOBUILD) $(2) -tags openh264,opus,netgo,osusergo \
	-ldflags "$(GO_LDFLAGS) -linkmode external -extldflags -static" \
	-o $@ ./cmd/robin-agent
endef

.PHONY: all agents cli dist repro go-version test test-agents test-js test-short test-x test-wayland check e2e fuzz clean
.PHONY: cover cover-unit cover-x cover-wayland cover-e2e cover-report cover-baseline
.PRECIOUS: bin/robin-agent-linux-% bin/cover/robin-agent-linux-%

all: cli

cli: bin/robin

agents: $(foreach a,$(ARCHES),$(AGENTS)/robin-agent-linux-$(a).gz)

# Every shipped binary is built by the Go that go.mod names, or not at all.
go-version:
	@v=$$($(GO) env GOVERSION); [ "$$v" = "$(GOTOOLCHAIN)" ] || \
		{ echo "make: go is $$v, go.mod wants $(GOTOOLCHAIN); unset GOTOOLCHAIN" >&2; exit 1; }

bin/robin: agents $(GOSRC) go.mod go.sum Makefile $(VERSION_FILE) | go-version
	CGO_ENABLED=0 $(GOBUILD) -ldflags "$(GO_LDFLAGS)" -o $@ ./cmd/robin

bin/robin-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh Makefile $(VERSION_FILE) | go-version
	$(call agent_build,$*)

# Go's gzip rather than the host's: implementations compress differently,
# and the CLI embeds this file byte for byte.
$(AGENTS)/robin-agent-linux-%.gz: bin/robin-agent-linux-%
	$(GO) run ./internal/tools/gz $< $@

# Release CLIs for every client platform, each embedding every agent. A
# release is a tagged commit, or whatever VERSION the caller names.
dist: agents | go-version
	@case "$(origin VERSION)" in command*|environment*) ;; *) \
		git describe --tags --match 'v[0-9]*' --exact-match >/dev/null 2>&1 || \
		{ echo "make dist: HEAD is not a release tag; pass VERSION=" >&2; exit 1; }; esac
	@set -e; echo "dist VERSION=$(VERSION)"; \
	for p in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do \
		os=$${p%/*}; arch=$${p#*/}; ext=; [ $$os != windows ] || ext=.exe; \
		echo "build dist/robin-$$os-$$arch$$ext"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GOBUILD) \
			-ldflags "$(GO_LDFLAGS)" -o dist/robin-$$os-$$arch$$ext ./cmd/robin; \
	done

# Builds the agents, the CLI and dist from this tree and again from a copy
# outside any repository, with its own home, temp directory, zig and Go
# build cache, and fails if any artifact differs. REPRO_TOOLCHAIN=1 also
# rebuilds openh264 and libopus for the copy instead of sharing them with
# it. REPRO_SELFTEST=1 builds the copy with another VERSION and passes only
# when the comparison notices.
repro:
	toolchain/repro.sh

# The linux/amd64 CLI the e2e client container runs.
bin/robin-linux-amd64: agents $(GOSRC) go.mod go.sum Makefile $(VERSION_FILE) | go-version
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -ldflags "$(GO_LDFLAGS)" -o $@ ./cmd/robin

# Unit and integration tests. The encoder packages are tested against the real
# openh264 and libopus on linux/amd64 hosts; everywhere else they build as stubs.
test:
	$(GO) test ./...
ifeq ($(HOST_OS)/$(HOST_ARCH),linux/amd64)
	@set -e; $(call zig_env,amd64) \
	$(GO) test -tags openh264,opus $(CGO_TESTS)
endif

# The agents this tree builds, scanned for anything of the builder: paths,
# an unpinned Go, a build setting the recipes do not set. The test runs
# only here, against agents just built: make test stays free of the
# toolchain, and agents an older build left behind would not match.
test-agents: agents
	ROBIN_REQUIRE_AGENTS=1 $(GO) test ./internal/agentbin

# The page's pure modules, under Node's own test runner. make test runs them
# too, through TestPageModules in internal/web; this target is for make cover
# and CI. The files are named: Node 22 and later take a directory given to
# --test for a script and fail.
test-js:
	node --test internal/web/static/test/*.test.js

# Every Go fuzz target for FUZZTIME each, one after another, then the page's
# property tests with PROPERTY_RUNS inputs each. go test replays each
# target's seeds and testdata/fuzz on every run; a failing input found here
# is written there, to be committed with its fix.
FUZZTIME ?= 10s
PROPERTY_RUNS ?= 20000
fuzz:
	@set -e; for file in $$(grep -rl --include='*_test.go' '^func Fuzz' cmd internal | sort); do \
		pkg=./$$(dirname $$file); \
		for target in $$(sed -n 's/^func \(Fuzz[A-Za-z0-9_]*\)(.*/\1/p' $$file); do \
			echo "fuzz $$pkg $$target"; \
			$(GO) test -run '^$$' -fuzz "^$$target$$" -fuzztime $(FUZZTIME) $$pkg; \
		done; \
	done
	ROBIN_PROPERTY_RUNS=$(PROPERTY_RUNS) node --test internal/web/static/test/*.property.test.js

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
check: test test-agents test-x test-wayland e2e

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
bin/cover/robin-agent-linux-%: $(GOSRC) go.mod go.sum toolchain/*.sh Makefile $(VERSION_FILE)
	$(call agent_build,$*,-cover)

bin/cover/agents/robin-agent-linux-%.gz: bin/cover/robin-agent-linux-%
	@mkdir -p $(@D)
	$(GO) run ./internal/tools/gz $< $@

bin/cover/robin-linux-amd64: bin/cover/agents/robin-agent-linux-amd64.gz $(GOSRC) go.mod go.sum $(VERSION_FILE)
	printf '{"Replace":{"%s":"%s"}}\n' $(CURDIR)/$(AGENTS)/robin-agent-linux-amd64.gz $(CURDIR)/$< >bin/cover/overlay.json
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GOBUILD) -cover -overlay bin/cover/overlay.json \
		-ldflags "$(GO_LDFLAGS)" -o $@ ./cmd/robin

clean:
	rm -rf bin dist .repro $(AGENTS)/*.gz
