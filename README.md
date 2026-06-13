# PacketLoss

Ranks internet transit/ISP networks by how well they reach the top destinations per country,
using RIPE Atlas RTT + loss measurements. Static site. **See [PRODUCT.md](./PRODUCT.md) for the
full design.**

## Architecture

```
proto/ ──buf generate──▶ Go types (builder) + TS types (web)
                         │
config/*.yaml ─▶ builder (Go, stateless) ─▶ data/json/ (protojson)
                     ▲                            │
                RIPE Atlas (the store)        web (Astro) ─▶ dist/ (static)
```

- **builder/** (Go) — stateless: RIPE Atlas probe coverage → ensure ongoing ping measurements →
  fetch rolling window → score → JSON export. No local DB; RIPE retains the history.
- **web/** (Astro) — reads the `data/json/` protojson artifacts only → static HTML + SVG charts.
- **proto/** — the JSON contract, shared via `buf` (`protoc-gen-go` + `protobuf-es`).

## Commands (`make help`)

```
make install        # web deps (incl. protoc-gen-es, needed for codegen)
make build          # buf generate + go build + astro build
make build-builder  # gen + go mod tidy + compile the Go builder
make build-web      # render the static site from data/json/
make run             # full pipeline: RIPE collect -> score -> export -> render
make stop-measurements  # stop all packetloss measurements on your RIPE account (reset)
make web-dev         # Astro dev server against current data/json/
make lint            # buf lint
make check           # lint + compile builder
```

First time: `make install`, `cp .env.example .env` and add your key, then `make run`.

## Data

All data is real RIPE Atlas — no synthetic/demo mode, and the builder keeps **no local state**
(RIPE retains the measurement history). Each hourly `make run`:
1. resolves live probe coverage per provider (gates out networks with `< min_probes`);
2. ensures one ongoing ping measurement per **distinct target** (across all countries that
   include it), reuse-by-description else create — keeps us at ~1 measurement/target vs RIPE's
   25/target cap;
3. fetches the rolling window from RIPE, attributes each result to its (country, provider) by the
   probe's country + ASN, scores, and exports `data/json/`.

Measurement **creation** needs a key with the *"Create a new user-defined measurement"*
permission (+ credits); a read-only key yields real coverage but empty RTT/loss. Results lag the
first run — RIPE needs an interval to execute new measurements, so RTT/loss appears on a later
run. RIPE caps concurrent measurements per target; `make stop-measurements` clears ours to reset.

## Config & env

- `config/thresholds.yaml` — scoring window, loss weight K, min probes, status cutoffs.
- `config/<country>.yaml` — providers (curated from bgp.tools cone rank) + targets. Each target
  is a pinned `address:` (IP) or a `host:` (resolved per-probe at runtime). DE is wired so far.
- `.env` (loaded via dotenv, gitignored) — `RIPE_ATLAS_API_KEY`. See `.env.example`.

## TODO

- Add NL/FR/AT country configs.
- Optional `uPlot` island for the provider drill-down (currently a static table).
