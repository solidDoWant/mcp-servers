SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -euc

BIN_DIR := bin
SERVERS := $(notdir $(wildcard servers/*))
VERSION ?= 0.0.0-dev

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

.PHONY: servers
servers: ## List the servers.
	@printf '%s\n' $(SERVERS)

##@ Development

.PHONY: fmt
fmt: ## Format Go code.
	golangci-lint fmt ./...

.PHONY: lint
lint: ## Run every check (golangci-lint, go mod tidy, shellcheck, image references, tracking IDs), then fail if any failed.
	@failed=""; \
	golangci-lint run ./... || failed="$$failed golangci-lint"; \
	go mod tidy -diff || failed="$$failed go-mod-tidy"; \
	shellcheck scripts/*.sh || failed="$$failed shellcheck"; \
	./scripts/check-images.sh || failed="$$failed images"; \
	./scripts/check-tracking-ids.sh || failed="$$failed tracking-ids"; \
	if [ -n "$$failed" ]; then echo "lint failed:$$failed" >&2; exit 1; fi

.PHONY: test
test: ## Run the unit tests (race detector), which include each server's tool-list golden file.
	timeout 10m go test -race -count=1 ./...

.PHONY: test-live
test-live: ## Run the tests that call the real upstream services (build tag `live`). CI runs them nightly.
	timeout 10m go test -tags live -count=1 -run 'Live' ./...

.PHONY: golden
golden: ## Rewrite every server's tool-list golden file. Review the diff: clients see every word.
	go test -count=1 -run 'ToolList' ./servers/... -args -update

.PHONY: vulncheck
vulncheck: ## Report known vulnerabilities in dependencies and the standard library.
	govulncheck ./...

##@ Build

.PHONY: build
build: ## Build every server into bin/.
	@mkdir -p "$(BIN_DIR)"
	@for s in $(SERVERS); do \
		echo "go build $$s"; \
		CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=$(VERSION)" -o "$(BIN_DIR)/$$s" "./servers/$$s"; \
	done

.PHONY: images
images: ## Check that every server's image builds for linux/amd64 and arm64, without pushing (SERVER=name for one).
	@for s in $(or $(SERVER),$(SERVERS)); do ./scripts/image.sh "$$s" "$(VERSION)" false; done

.PHONY: clean
clean: ## Remove build output.
	rm -rf "$(BIN_DIR)" build
