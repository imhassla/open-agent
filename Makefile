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

.PHONY: build install test race build-treesitter install-treesitter test-treesitter

build:
	go build -o open-agent .

install:
	go build -o $(BIN) .

test:
	go test ./...

race:
	go test -race ./...

# --- tree-sitter variant (CGo; needs clang/gcc) ---
build-treesitter:
	go build -tags treesitter -o open-agent .

install-treesitter:
	go build -tags treesitter -o $(BIN) .

test-treesitter:
	go test -tags treesitter ./...
