# open-agent — a single-binary multi-model agentic CLI.
# The default build is pure Go (permissive-licensed deps: go-git, yaegi, go-diff).
# The `treesitter` variant adds CGo tree-sitter for richer multi-language repo maps
# (requires a C toolchain).

# Install destination. Defaults to `go env GOBIN`, else $(go env GOPATH)/bin.
GOBIN ?= $(shell go env GOBIN)
ifeq ($(GOBIN),)
GOBIN := $(shell go env GOPATH)/bin
endif
BIN ?= $(GOBIN)/open-agent
# Dev binary: a DISTINCT name so it never shadows (or gets shadowed by) the
# Homebrew-installed `open-agent` at /opt/homebrew/bin. Invoke it as
# `open-agent-dev`; its `version` output is tagged `open-agent-dev` too.
DEVBIN ?= $(GOBIN)/open-agent-dev

.PHONY: build install dev dev-build test race preflight build-treesitter install-treesitter test-treesitter

build:
	go build -o open-agent .

install:
	go build -o $(BIN) .

# dev — build + install the dev binary as `open-agent-dev` (own name, own version
# label). Use this to test local changes WITHOUT touching the brew-managed
# `open-agent`. Run: `open-agent-dev <verb> ...`.
dev:
	go build -ldflags "-X main.devLabel=dev" -o $(DEVBIN) .
	@echo "installed $(DEVBIN)  —  run as: open-agent-dev version"

# dev-build — same dev binary, left in the working tree as ./open-agent-dev
# (not installed), for a throwaway local run.
dev-build:
	go build -ldflags "-X main.devLabel=dev" -o open-agent-dev .

test:
	go test ./...

race:
	go test -race ./...

# preflight — the pre-publication regression ritual: full test suite, then the
# execution-grounded bench matrix across the three main worker families (live
# OpenRouter calls, ~$0.15, several minutes). Publish only on a green preflight;
# a family regressing here means a change is family-specific or broke routing.
preflight: install
	go test ./...
	$(BIN) bench --families qwen,glm,minimax --max-cost 0.08

# --- tree-sitter variant (CGo; needs clang/gcc) ---
build-treesitter:
	go build -tags treesitter -o open-agent .

install-treesitter:
	go build -tags treesitter -o $(BIN) .

test-treesitter:
	go test -tags treesitter ./...
