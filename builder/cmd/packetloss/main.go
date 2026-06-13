// Command packetloss is the builder for the PacketLoss pipeline. It runs in one of
// two modes, each a distinct `make` step:
//
//	-mode atlas  sync local config to RIPE Atlas: resolve probe coverage per provider
//	             ASN and ensure ONE ongoing ping measurement per distinct target
//	             (reuse-by-description else create; reconcile its probe roster).
//	-mode data   download the rolling window of results from those measurements,
//	             attribute each to its (country, provider) by probe, score, and write
//	             the JSON cache the web layer reads.
//
// One measurement per target keeps us far under RIPE's cap of 25 measurements to the
// same target, regardless of how many countries we add. See PRODUCT.md "Build pipeline".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/joho/godotenv"

	"github.com/exys/packetloss/builder/internal/config"
	"github.com/exys/packetloss/builder/internal/export"
	"github.com/exys/packetloss/builder/internal/ripe"
	"github.com/exys/packetloss/builder/internal/score"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("packetloss: %v", err)
	}
}

// probeMeta attributes a probe to the (country, provider) it was selected for.
type probeMeta struct {
	country string
	asn     uint32
}

// countryState accumulates per-country coverage + attributed results.
type countryState struct {
	probeCounts map[uint32]int // asn -> true connected-probe count
	probeIDs    []uint32       // covered probes in this country
	results     []score.Result
}

// globalTarget is one distinct destination, measured once across all countries
// whose config includes it.
type globalTarget struct {
	id             string
	endpoint       string
	resolveOnProbe bool
	countries      map[string]bool
	probeIDs       []uint32
}

func run() error {
	var (
		configDir = flag.String("config", "config", "config dir (thresholds.yaml + <country>.yaml)")
		jsonDir   = flag.String("json", "data/json", "local cache dir for scored JSON artifacts (data mode)")
		mode      = flag.String("mode", "", "pipeline step: 'atlas' (sync config -> RIPE measurements) or 'data' (download results -> JSON cache)")
	)
	flag.Parse()

	switch *mode {
	case "atlas", "data":
	default:
		return fmt.Errorf("set -mode to 'atlas' or 'data'")
	}

	ctx := context.Background()
	now := time.Now().UTC()

	// Load the RIPE Atlas API key from .env (repo root or builder/). Real env vars win.
	for _, p := range []string{".env", "../.env"} {
		_ = godotenv.Load(p)
	}
	apiKey := os.Getenv("RIPE_ATLAS_API_KEY")
	if apiKey == "" {
		log.Print("warning: RIPE_ATLAS_API_KEY not set — RIPE calls will fail or return empty")
	}
	rc := ripe.New(apiKey)

	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	log.Printf("config: %d countries, window=%dd, min_probes=%d",
		len(cfg.Countries), cfg.Thresholds.WindowDays, cfg.Thresholds.MinProbes)

	// Shared across both modes: resolve probe coverage per (country, provider), then
	// dedupe targets across countries into one global target each.
	states, probeInfo, errs := resolveCoverage(ctx, rc, cfg)
	targets, order, err := buildTargets(cfg, states)
	if err != nil {
		return err
	}

	switch *mode {
	case "atlas":
		errs = append(errs, syncAtlas(ctx, rc, cfg, targets, order)...)
	case "data":
		window := now.AddDate(0, 0, -cfg.Thresholds.WindowDays)
		errs = append(errs, collectData(ctx, rc, cfg, states, probeInfo, targets, order, window, now, *jsonDir)...)
	}

	if len(errs) > 0 {
		return fmt.Errorf("%s step had errors:\n%w", *mode, errors.Join(errs...))
	}
	return nil
}

// resolveCoverage resolves connected RIPE probes per provider ASN, gating out
// under-covered providers, and records the (country, provider) each probe is
// attributed to. Used by both modes.
func resolveCoverage(ctx context.Context, rc *ripe.Client, cfg *config.Config) (map[string]*countryState, map[uint32]probeMeta, []error) {
	var errs []error
	states := map[string]*countryState{}
	probeInfo := map[uint32]probeMeta{}
	for _, c := range cfg.Countries {
		st := &countryState{probeCounts: map[uint32]int{}}
		states[c.Code] = st
		for _, p := range c.Providers {
			if len(p.ProbeIDs) > 0 {
				// Explicit probe override: attribute these probes to this provider, skip ASN lookup.
				st.probeCounts[p.ASN] = len(p.ProbeIDs)
				for _, id := range p.ProbeIDs {
					probeInfo[id] = probeMeta{country: c.Code, asn: p.ASN}
					st.probeIDs = append(st.probeIDs, id)
				}
				log.Printf("%s AS%d: using %d override probe(s): %v", c.Code, p.ASN, len(p.ProbeIDs), p.ProbeIDs)
				continue
			}
			probes, total, err := rc.ProbesByASN(ctx, p.ASN, c.Code, cfg.Thresholds.ProbesPerProvider)
			if err != nil {
				log.Printf("%s AS%d: probes: %v", c.Code, p.ASN, err)
				errs = append(errs, fmt.Errorf("%s AS%d probes: %w", c.Code, p.ASN, err))
				continue
			}
			st.probeCounts[p.ASN] = total // true connected-probe count, not the capped sample
			if total < cfg.Thresholds.MinProbes {
				continue // under-covered: exclude from measurements (normal)
			}
			for _, pr := range probes {
				probeInfo[pr.ID] = probeMeta{country: c.Code, asn: p.ASN}
				st.probeIDs = append(st.probeIDs, pr.ID)
			}
		}
	}
	return states, probeInfo, errs
}

// buildTargets dedupes targets across countries into one global target per id,
// spanning the covered probes of every country that includes it.
func buildTargets(cfg *config.Config, states map[string]*countryState) (map[string]*globalTarget, []string, error) {
	targets := map[string]*globalTarget{}
	var order []string
	for _, c := range cfg.Countries {
		for _, t := range c.Targets {
			ep, rop := t.Endpoint()
			gt := targets[t.ID]
			if gt == nil {
				gt = &globalTarget{id: t.ID, endpoint: ep, resolveOnProbe: rop, countries: map[string]bool{}}
				targets[t.ID] = gt
				order = append(order, t.ID)
			} else if gt.endpoint != ep {
				return nil, nil, fmt.Errorf("target %q has conflicting endpoints across countries (%q vs %q)", t.ID, gt.endpoint, ep)
			}
			gt.countries[c.Code] = true
			gt.probeIDs = append(gt.probeIDs, states[c.Code].probeIDs...)
		}
	}
	return targets, order, nil
}

// syncAtlas (mode=atlas) ensures one ongoing ping measurement per distinct target and
// reconciles its probe roster to today's config. It creates/updates RIPE state but
// fetches no results — RIPE needs an interval to execute new measurements before the
// data step can read them.
func syncAtlas(ctx context.Context, rc *ripe.Client, cfg *config.Config, targets map[string]*globalTarget, order []string) []error {
	if rc.APIKey == "" {
		return []error{fmt.Errorf("atlas: RIPE_ATLAS_API_KEY required to ensure measurements")}
	}
	var errs []error
	for _, id := range order {
		gt := targets[id]
		if len(gt.probeIDs) == 0 {
			log.Printf("target %s: no covered probes — skipping", id)
			continue
		}
		if len(gt.probeIDs) > 1000 {
			// RIPE caps a measurement at 1000 probes; chunk before we ever hit this.
			log.Printf("target %s: %d probes exceeds RIPE's 1000/measurement cap — chunking is a TODO", id, len(gt.probeIDs))
		}
		desc := "packetloss " + id
		msmID, created, err := rc.EnsureMeasurement(ctx, gt.endpoint, desc, gt.resolveOnProbe, gt.probeIDs, cfg.Thresholds.MeasurementIntervalSecond, cfg.Thresholds.Packets)
		if err != nil {
			log.Printf("target %s: ensure: %v", id, err)
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if created {
			log.Printf("target %s: created measurement %d (%d probes, %d countries)", id, msmID, len(gt.probeIDs), len(gt.countries))
			continue
		}
		log.Printf("target %s: reusing measurement %d", id, msmID)
		// Reconcile probe roster to match today's config. New adds participate on
		// the next interval tick (RIPE has no immediate-trigger API).
		added, removed, err := rc.ReconcileProbes(ctx, msmID, gt.probeIDs)
		switch {
		case err != nil:
			log.Printf("target %s: reconcile FAILED (intended +%d -%d): %v", id, len(added), len(removed), err)
			errs = append(errs, fmt.Errorf("%s reconcile: %w", id, err))
		case len(added) > 0 || len(removed) > 0:
			log.Printf("target %s: reconciled msm %d: +%d (%v) / -%d (%v)", id, msmID, len(added), added, len(removed), removed)
		}
	}
	return errs
}

// collectData (mode=data) locates each target's measurement (created by the atlas
// step), fetches the rolling window, attributes results to (country, provider), scores
// in-memory, and writes the JSON cache the web layer reads.
func collectData(ctx context.Context, rc *ripe.Client, cfg *config.Config, states map[string]*countryState, probeInfo map[uint32]probeMeta, targets map[string]*globalTarget, order []string, window, now time.Time, jsonDir string) []error {
	var errs []error
	if rc.APIKey == "" {
		errs = append(errs, fmt.Errorf("data: RIPE_ATLAS_API_KEY required to list/fetch measurements"))
	}
	msmIDs := map[string]int64{} // target id -> measurement id, for the web source link
	for _, id := range order {
		gt := targets[id]
		if rc.APIKey == "" || len(gt.probeIDs) == 0 {
			continue
		}
		desc := "packetloss " + id
		msmID, found, err := rc.FindMeasurement(ctx, desc)
		if err != nil {
			log.Printf("target %s: find: %v", id, err)
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if !found {
			log.Printf("target %s: no measurement yet — run 'make atlas' first", id)
			continue
		}
		msmIDs[id] = msmID
		pings, err := rc.FetchResults(ctx, msmID, window)
		if err != nil {
			log.Printf("target %s: fetch: %v", id, err)
			errs = append(errs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		var n int
		for _, r := range pings {
			info, ok := probeInfo[r.PrbID]
			if !ok || !gt.countries[info.country] {
				continue // probe not tracked, or its country doesn't include this target
			}
			st := states[info.country]
			st.results = append(st.results, score.Result{
				ProviderASN: info.asn, TargetID: id, ProbeID: r.PrbID,
				TS:     time.Unix(r.Timestamp, 0).UTC(),
				RTTMin: nonNeg(r.Min), RTTAvg: nonNeg(r.Avg), RTTMax: nonNeg(r.Max),
				LossPct: r.LossPct(),
			})
			n++
		}
		if n > 0 {
			log.Printf("target %s: %d samples (msm %d)", id, n, msmID)
		}
	}

	// Score + export per country into the local cache.
	var reps []score.CountryReport
	for _, c := range cfg.Countries {
		st := states[c.Code]
		rep := score.Compute(c, cfg.Thresholds, st.probeCounts, st.results, now)
		reps = append(reps, rep)
		log.Printf("scored %s: %d providers, %d samples in window", c.Code, len(rep.Providers), len(st.results))
	}
	for _, rep := range reps {
		if err := export.WriteCountry(jsonDir, rep, msmIDs); err != nil {
			return append(errs, err)
		}
	}
	if err := export.WriteCountryList(jsonDir, reps, now); err != nil {
		return append(errs, err)
	}
	log.Printf("exported JSON artifacts to %s", jsonDir)
	return errs
}

// nonNeg clamps RIPE's -1 "no RTT" sentinel (fully-lost ping) to 0.
func nonNeg(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}
