# PacketLoss — build orchestration. See PRODUCT.md for the design.
.DEFAULT_GOAL := help

# --- config (override on the command line, e.g. `make run CONFIG_DIR=...`) ---
CONFIG_DIR ?= config
DATA_DIR   ?= data
JSON_DIR   ?= $(DATA_DIR)/json

GO      ?= go
BUILDER := ./cmd/packetloss   # relative to builder/ (run cd's there first)

.PHONY: help install gen tidy build build-builder build-web lint breaking \
        run stop-measurements web-dev web-build clean check

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

install: ## Install web deps (incl. protoc-gen-es) so codegen can run
	cd web && pnpm install

gen: ## Generate Go + TS types from proto via buf
	buf generate

tidy: gen ## Resolve Go module deps (after codegen, so the pb package exists)
	cd builder && $(GO) mod tidy

lint: ## Lint the proto schema
	buf lint

breaking: ## Check proto for breaking changes against git HEAD
	buf breaking --against '.git#branch=HEAD'

build: build-builder build-web ## Generate types, build builder + web

build-builder: tidy ## Compile the Go builder (runs gen + tidy first)
	cd builder && $(GO) build ./...

build-web: ## Build the static site (reads $(JSON_DIR))
	cd web && DATA_DIR=$(abspath $(JSON_DIR)) pnpm build

# Full hourly pipeline: stateless RIPE collect -> score -> export, then static render.
run: build-builder ## Run the builder pipeline (RIPE -> JSON), then render the site
	cd builder && $(GO) run $(BUILDER) -config ../$(CONFIG_DIR) -json ../$(JSON_DIR)
	$(MAKE) build-web

stop-measurements: build-builder ## Stop ALL RIPE measurements created by packetloss (reset)
	cd builder && $(GO) run $(BUILDER) -stop

web-dev: ## Astro dev server against current $(JSON_DIR)
	cd web && DATA_DIR=$(abspath $(JSON_DIR)) pnpm dev

clean: ## Remove build outputs (keeps the json artifacts)
	rm -rf web/dist builder/bin

check: lint build-builder ## Lint proto + compile builder
