# PacketLoss

The pipeline is four distinct steps, run in order:

```
make atlas    # sync config -> RIPE Atlas measurements (ensure + reconcile probes)
make data     # download the rolling window of results -> local cache (data/json/)
make web      # build the static site from the cache -> web/dist
make publish  # upload web/dist to bunny.net (bunny CLI)
```
## Data

All data is real RIPE Atlas and the builder keeps **no local state**
(RIPE retains the measurement history). Each hourly `make run`:
1. resolves live probe coverage per provider (gates out networks with `< min_probes`);
3. fetches the rolling window from RIPE, attributes each result to its (country, provider) by the probe's country + ASN, scores, and exports `data/json/`.

Measurement **creation** needs a key so its run manually

## Config & env

- `config/thresholds.yaml` — scoring window, loss weight K, min probes, status cutoffs.
- `config/<country>.yaml` — providers (curated from bgp.tools cone rank) + targets. Each target
  is a pinned `address:` (IP) or a `host:` (resolved per-probe at runtime). DE is wired so far.
- `.env` (loaded via dotenv, gitignored) — `RIPE_ATLAS_API_KEY`. See `.env.example`.
