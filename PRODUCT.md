# PacketLoss

A static website that ranks internet networks by how well they reach the most important
destinations in a country, using real RIPE Atlas measurements of round-trip time and packet
loss. Each network gets a gamified 0–10 score and a green/red status grid; clean and bold in
the spirit of isbgpsafeyet.com, with the competitive framing of the Netflix ISP Speed Index.
A periodic build job pulls the rolling window of measurements from RIPE Atlas (which retains
the history), recomputes scores, and bakes static HTML. The builder is stateless — no local
database. This document is the spec to implement.

## Concepts

- **Provider** ("source"): a network we rank (rows in the grid). Seeded from bgp.tools cone
  rankings, curated in YAML.
- **Target** ("destination"): an important service/network we measure reachability to
  (columns). Eyeball nets from APNIC + named hyperscalers/clouds (Google, Netflix, Amazon,
  StackIT, SysEleven, …), curated in YAML.
- **Score**: 0–10, **relative to peers** per (country, target), like the Netflix index.
- **Status color**: green/red per grid cell, from **absolute** thresholds (so red = actually
  bad, not just "not #1").

## Scope (v1)

- Countries: **DE** (primary) + EU anchors **NL, FR, AT**. Per-country config.
- ~10–15 providers and ~15–20 targets per country.
- Metrics: **RTT + packet loss** via RIPE Atlas **ping**. (No traceroute in v1.)

## Attribution

- Method: **probe-in-ASN** — a provider's results come from RIPE Atlas probes inside its ASN.
- Curate the provider list to networks with **≥ N probes** (default N=2). Under-covered
  networks are listed with an "insufficient coverage" state, not scored.

## Measurements

- Hourly cadence. **One ongoing** ping measurement **per distinct target**, spanning the probes
  of every covered provider in every country that includes that target. Results are attributed
  back to `(country, provider)` by probe. This keeps us at ~1 measurement per target — far under
  RIPE's cap of 25 measurements to the same target — no matter how many countries we add (a
  per-`(country, target)` measurement would multiply by country count and hit the cap ~EU-wide).
  Defined once and reused (discovered by description); never one-shot per build. Up to 1000
  probes/measurement (chunk beyond that).
- ~3–5 probes per provider. Targets are **hostnames resolved per-probe at runtime**
  (`resolve_on_probe`), so each probe is measured at its nearest edge. Avoid pinning
  ultra-popular anycast IPs (`8.8.8.8`, `1.1.1.1`) — they hit RIPE's per-target
  concurrent-measurement cap. A pinned `address: <ip>` is supported for niche/own targets.
- Requires a RIPE Atlas account + credits and a key with measurement-creation permission.
  Cadence and probe count are the credit levers; both come from config.

## Scoring

Per (country, target), rolling window = **last 7 days**:

1. Per provider: window **median RTT** (p50, ms) and **mean loss %** across its probes.
2. `cost = rtt_p50_ms + loss_pct * K` (K = 10 ms per 1% loss, configurable).
3. Normalize relative to peers in that (country, target): lowest cost → **10**, scaled down
   linearly against the peer range.
4. **Provider country score** = mean of its per-target scores → the headline "x/10" badge.

Grid color (absolute): green if `loss_pct < 2%` and RTT within a sane band; else red (amber
optional). All thresholds/weights (window, K, color cutoffs, N) live in config.

## State (RIPE Atlas is the store)

The builder holds **no persistent state** — no SQLite, no S3. RIPE Atlas retains the results of
ongoing measurements and serves them by time range, so it *is* the history store. Each build:

- discovers our measurements by description (`packetloss <target>`) via the authenticated
  `/measurements/my/` list — no need to persist measurement ids;
- re-fetches the rolling window (last `window_days`) of results from RIPE and scores it
  in-memory;
- creating a measurement is idempotent: ensure = reuse-if-exists-by-description else create, so
  reruns (even concurrent) converge without duplicates and no lock is needed.

Trade-off (accepted): no offline resilience if RIPE's API is down during a build, and no archive
beyond RIPE's retention / a measurement's lifetime. Re-add a store later behind the same JSON
contract if long-term history is wanted.

## Build pipeline (idempotent, hourly)

**Builder** (`builder/`, Go — single binary, stateless):
1. Load + validate config (countries, providers, targets, thresholds) from YAML.
2. Resolve probes per provider ASN from RIPE Atlas; gate out under-covered providers (`< N`).
3. Dedupe targets across countries; ensure one ongoing ping measurement per distinct target over
   every including country's covered probes (reuse-by-description else create).
4. Fetch the rolling window of results from RIPE; attribute each to its `(country, provider)` by
   the probe's country + ASN.
5. Compute scores in-memory.
6. **Export JSON artifacts** for the web layer into `data/json/` (see below).

Collection errors (RIPE/network) are aggregated and re-raised as a non-zero exit, but the build
still scores/exports what it has.

**Web** (`web/`, Astro): `astro build` reads the JSON artifacts and renders the three page types
+ SVG charts into `dist/`.

**Publish:** deploy `dist/` to the static host.

One hourly job, sequential: Go builder (writes `data/json/`) → `astro build` (reads `data/json/`)
→ deploy `dist/`. Every stage is idempotent and safe to re-run.

## JSON artifacts (builder → web)

The builder writes a static JSON tree to `data/json/`; this is the **only** contract the web
layer depends on. Shape mirrors the pages:

```
data/json/
  countries.json                       # [{code, name, provider_count, …}]
  <country>/overview.json              # grid: providers × targets → {score, status, rtt_p50, loss}
  <country>/providers/<asn>.json       # detail: score, per-target time series, last 30 measurements
  <country>/targets/<id>.json          # comparison: per-provider time series for one target
```

The schema is defined once in **Protocol Buffers** (`proto/`) — the single source of truth.
`buf generate` emits Go types (`protoc-gen-go`) for the builder and TS types
(`protobuf-es` / `@bufbuild/protobuf`) for the web. Files are serialized as **protojson**:
`protojson.Marshal` in Go on write, `fromJson` in TS for optional build-time validation.
`buf lint` + `buf breaking` guard the contract so the two sides can't drift silently.

Schema notes: use `int32`/`uint32` for ids and ASNs so they stay JSON numbers (protojson
encodes 64-bit ints as strings); timestamps as RFC3339 strings; JSON field names are camelCase.
Time-series payloads stay lean (downsampled to what the charts need).

## Pages

1. **Country overview grid** — rows = providers, cols = targets, cells = green/red status.
   Sortable by score, country switcher (DE/NL/FR/AT). Landing page.
2. **Provider detail** — name + big **SCORE: x/10** badge; one small RTT+loss-over-time chart
   per target; click a target → **last 30 measurements** table. Under-covered providers show
   the coverage state instead of numbers.
3. **Target comparison** — per-target charts overlaying multiple providers (e.g. DTAG red vs.
   Inter.link green) to show best vs. worst.

Charts: build-time **SVG** small multiples via Astro components (static, no JS). The detail
drill-down is a `uPlot` Astro island fed by the provider's JSON artifact.

## Project structure

```
proto/packetloss/v1/*.proto   # JSON contract schema — single source of truth
buf.yaml  buf.gen.yaml         # buf lint/breaking + codegen config
config/                       # per-country YAML: providers, targets; plus thresholds.yaml
builder/                      # Go module — stateless collector, scorer, JSON exporter
  cmd/packetloss/main.go      # CLI entrypoint / orchestrator (also `-stop` to reset)
  internal/config/            # YAML load + validate
  internal/ripe/              # RIPE Atlas REST client (probes, measurements, results)
  internal/score/             # scoring (pure, no I/O)
  internal/export/            # protojson artifact writer
  internal/pb/                # generated Go types (buf → protoc-gen-go)
  go.mod
web/                          # Astro static site — consumes JSON only
  src/pages/                  # overview grid, provider detail, target comparison
  src/components/             # SVG chart components + uPlot island
  src/gen/                    # generated TS types (buf → protobuf-es)
data/json/                    # builder-generated protojson artifacts (web reads these)
dist/                         # astro build output, deployable static site
```

## Stack

Go for everything but the frontend; Astro for the frontend. The two meet only at the JSON
artifacts in `data/json/` — no shared runtime, no DB anywhere.

- **Builder:** Go, single static binary, stateless. RIPE Atlas via `net/http` against the REST
  API. Key loaded from `.env` via `joho/godotenv`. YAML via `gopkg.in/yaml.v3`. No SQLite, no
  AWS SDK.
- **Frontend / renderer:** Astro, static output. Imports the JSON tree at build time → static
  HTML. No DB driver.
- **Charts:** server-side SVG via `d3-shape`/`d3-scale` (path math only, no DOM) inside Astro
  components; interactive drill-down = a `uPlot` island over the same JSON.
- **Contract:** Protobuf schema in `proto/`, managed with **buf**. `buf generate` → Go types
  (`protoc-gen-go`) + TS types (`protobuf-es`); artifacts serialized as protojson. `buf lint` /
  `buf breaking` enforce the contract across builder and web.
- **Tooling:** Go toolchain for the builder, pnpm/Node for Astro, buf for codegen; cron run.

Go (not TS) for the builder: the web layer shares no code with it (the contract is JSON), so the
builder stays a self-contained Go binary — fast RIPE Atlas fan-out, trivial single-binary cron
deploy.

## Deferred (v2)

Traceroute / AS-path attribution to score pure-transit carriers mid-path; interactive
charts; alerting; history export.
