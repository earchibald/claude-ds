# claude-ds — Go rewrite build targets.
#
# Static binary build (matches release.yml):
#   CGO_ENABLED=0 go build -ldflags="-s -w"
#
# Local smoke-test build emits to bin/claude-ds-test (gitignored) so it
# never overwrites the Bash launcher named `claude-ds` at the repo root.

BINARY      ?= bin/claude-ds-test
LDFLAGS     ?= -s -w
GO          ?= go
GOOS        ?= $(shell $(GO) env GOOS)
GOARCH      ?= $(shell $(GO) env GOARCH)

.PHONY: build
build: ## Static-binary build to $(BINARY)
	CGO_ENABLED=0 $(GO) build -ldflags="$(LDFLAGS)" -o $(BINARY) ./...

.PHONY: smoke
smoke: build ## Quick acceptance-criteria smoke test
	@echo ">>> --version"
	@$(BINARY) --version
	@echo ">>> --help (head)"
	@$(BINARY) --help | head -20
	@echo ">>> -- foo bar (passthrough; expects TODO line)"
	@$(BINARY) -- foo bar || true

.PHONY: tidy
tidy: ## Refresh go.sum
	$(GO) mod tidy

.PHONY: clean
clean: ## Remove local build artifacts
	rm -rf bin/

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | awk 'BEGIN {FS = ":.*?## "}; {printf "%-12s %s\n", $$1, $$2}'
