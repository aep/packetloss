# PacketLoss — four-step pipeline: atlas -> data -> web -> publish.
# See PRODUCT.md for the design.
.DEFAULT_GOAL := help

# --- config (override on the command line, e.g. `make data JSON_DIR=...`) ---
CONFIG_DIR ?= config
DATA_DIR   ?= data
JSON_DIR   ?= $(DATA_DIR)/json

GO      ?= go
BUILDER := ./cmd/packetloss   # relative to builder/ (recipes cd there first)

.PHONY: help install gen tidy lint atlas data web publish

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# --- plumbing (run when the proto contract or deps change; the pipeline steps
#     below use the committed generated code and don't need these each run) ---
install: ## Install web deps (incl. protoc-gen-es for codegen)
	cd web && pnpm install

gen: ## Regenerate Go + TS types from proto via buf
	buf generate

tidy: gen ## Resolve Go module deps (after codegen, so the pb package exists)
	cd builder && $(GO) mod tidy

lint: ## Lint the proto schema
	buf lint

# --- the pipeline ---

atlas: ## Sync local config to RIPE Atlas measurements (ensure + reconcile probes)
	cd builder && $(GO) run $(BUILDER) -mode atlas -config ../$(CONFIG_DIR)

data: ## Download measurement results from RIPE into the local cache ($(JSON_DIR))
	cd builder && $(GO) run $(BUILDER) -mode data -config ../$(CONFIG_DIR) -json ../$(JSON_DIR)

web: ## Build the static site from the local cache into web/dist
	cd web && DATA_DIR=$(abspath $(JSON_DIR)) pnpm build

publish: ## Upload web/dist to a bunny.net Storage Zone (HTTP API; reads BUNNY_* from .env)
	@test -d web/dist || { echo "web/dist not found — run 'make web' first"; exit 1; }
	@set -a; [ -f .env ] && . ./.env; set +a; \
	  : "$${BUNNY_STORAGE_ZONE:?set BUNNY_STORAGE_ZONE in .env}"; \
	  : "$${BUNNY_STORAGE_KEY:?set BUNNY_STORAGE_KEY in .env}"; \
	  host=$${BUNNY_STORAGE_ENDPOINT:-storage.bunnycdn.com}; \
	  cd web/dist && find . -type f | sed 's|^\./||' | while IFS= read -r f; do \
	    printf '  -> %s\n' "$$f"; \
	    curl -sS -f -X PUT \
	      -H "AccessKey: $$BUNNY_STORAGE_KEY" \
	      -H "Content-Type: application/octet-stream" \
	      --data-binary @"$$f" \
	      "https://$$host/$$BUNNY_STORAGE_ZONE/$$f" || exit 1; \
	  done; \
	  echo "published $$(find . -type f | wc -l) files to bunny zone '$$BUNNY_STORAGE_ZONE'"
