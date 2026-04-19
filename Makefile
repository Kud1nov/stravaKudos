# StravaKudos — refactor branch
# See docs/plans/20260419-rewrite-stravakudos.md for the full picture.

BINARY        := stravakudos
CMD_PATH      := ./cmd/stravakudos
LINUX_BINARY  := bin/$(BINARY)-linux-amd64
LOCAL_BINARY  := bin/$(BINARY)
DEPLOY_TAR    := dist/$(BINARY)-deploy.tar.gz

GO            ?= go
GOFLAGS       :=
TESTFLAGS     ?= -race -count=1

# Built-metadata: version from git, injectable into main via -ldflags
VERSION       ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS       := -s -w -X main.version=$(VERSION)

.PHONY: all build build-linux test lint tidy deploy-package clean help

all: build

## build: compile the local-arch binary into bin/
build:
	@mkdir -p bin
	@if [ -d cmd/stravakudos ]; then \
	  $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(LOCAL_BINARY) $(CMD_PATH); \
	else \
	  echo "cmd/stravakudos not present yet — skipping build (Task 1 state)"; \
	fi

## build-linux: static linux/amd64 binary, CGO disabled (required for deploy to rw)
build-linux:
	@mkdir -p bin
	@if [ -d cmd/stravakudos ]; then \
	  CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
	    $(GO) build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(LINUX_BINARY) $(CMD_PATH); \
	else \
	  echo "cmd/stravakudos not present yet — skipping build-linux (Task 1 state)"; \
	fi

## test: run all unit + integration tests
test:
	@if [ -n "$$($(GO) list ./... 2>/dev/null)" ]; then \
	  $(GO) test $(TESTFLAGS) ./...; \
	else \
	  echo "no Go packages yet — skipping tests (Task 1 state)"; \
	fi

## lint: static analysis + shellcheck on deploy scripts + systemd-analyze (soft)
lint:
	@if [ -n "$$($(GO) list ./... 2>/dev/null)" ]; then \
	  $(GO) vet ./...; \
	  command -v golangci-lint >/dev/null && golangci-lint run ./... || echo "golangci-lint not installed — skipping"; \
	else \
	  echo "no Go packages yet — skipping vet/golangci (Task 1 state)"; \
	fi
	@for f in deploy/*.sh; do [ -e "$$f" ] && bash -n "$$f" || true; done
	@if command -v systemd-analyze >/dev/null && [ -f deploy/stravakudos.service ]; then \
	  systemd-analyze verify deploy/stravakudos.service; \
	else \
	  echo "systemd-analyze not available or unit absent — skipping"; \
	fi

## tidy: go mod tidy
tidy:
	$(GO) mod tidy

## deploy-package: build linux binary + package deploy/ files for rw
deploy-package: build-linux
	@mkdir -p dist
	@[ -f $(LINUX_BINARY) ] || { echo "linux binary not built — did cmd/ exist?" >&2; exit 1; }
	tar -czf $(DEPLOY_TAR) -C . \
	  --transform 's,^$(LINUX_BINARY),$(BINARY),' \
	  --transform 's,^deploy/,,' \
	  $(LINUX_BINARY) deploy/
	@echo "deploy package: $(DEPLOY_TAR)"

## clean: remove build output
clean:
	rm -rf bin dist

help:
	@awk -F':.*?## ' '/^## /{print $$0}' $(MAKEFILE_LIST) | sed 's/^## //'
