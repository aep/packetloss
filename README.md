# PacketLoss

The pipeline is four distinct steps, run in order:

```
make atlas    # sync config -> RIPE Atlas measurements (ensure + reconcile probes)
make data     # download the rolling window of results -> local cache (data/json/)
make web      # build the static site from the cache -> web/dist
make publish  # upload web/dist to bunny.net (bunny CLI)
```

Plumbing (only after editing the proto contract, or on first setup):

```
make install  # web deps (incl. protoc-gen-es, needed for codegen)
make gen      # buf generate (Go + TS types)
make tidy     # buf generate + go mod tidy
make lint     # buf lint
```

First time: `make install`, `cp .env.example .env` and fill in your keys. Then `make atlas`
once to create the measurements — RIPE needs an interval to execute them, so RTT/loss appears
on a later `make data && make web && make publish`.

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
