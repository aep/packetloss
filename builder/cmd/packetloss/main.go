// Command packetloss is the stateless hourly build job: resolve RIPE Atlas probe
// coverage, ensure ONE ongoing ping measurement per distinct target (spanning every
// country's probes), fetch the rolling window of results, attribute each to its
// (country, provider) by probe, score, and export JSON. RIPE Atlas is the store —
// no local DB. One measurement per target keeps us far under RIPE's cap of 25
// measurements to the same target, regardless of how many countries we add.
// See PRODUCT.md "Build pipeline".
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
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
		jsonDir   = flag.String("json", "data/json", "output dir for JSON artifacts")
		stop      = flag.Bool("stop", false, "stop all RIPE measurements created by packetloss, then exit")
	)
	flag.Parse()

	ctx := context.Background()
	now := time.Now().UTC()

	// Load the RIPE Atlas API key from .env (repo root or builder/). Real env vars win.
	for _, p := range []string{".env", "../.env"} {
		_ = godotenv.Load(p)
	}
	apiKey := os.Getenv("RIPE_ATLAS_API_KEY")
	rc := ripe.New(apiKey)

	if *stop {
		return stopAll(ctx, rc)
	}
	if apiKey == "" {
		log.Print("warning: RIPE_ATLAS_API_KEY not set — cannot ensure/list measurements; output will be empty")
	}

	cfg, err := config.Load(*configDir)
	if err != nil {
		return err
	}
	log.Printf("config: %d countries, window=%dd, min_probes=%d",
		len(cfg.Countries), cfg.Thresholds.WindowDays, cfg.Thresholds.MinProbes)
	window := now.AddDate(0, 0, -cfg.Thresholds.WindowDays)

	var collectErrs []error

	// Phase 1: resolve probe coverage per (country, provider).
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
				collectErrs = append(collectErrs, fmt.Errorf("%s AS%d probes: %w", c.Code, p.ASN, err))
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

	// Phase 2: dedupe targets across countries -> one global target per id.
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
				return fmt.Errorf("target %q has conflicting endpoints across countries (%q vs %q)", t.ID, gt.endpoint, ep)
			}
			gt.countries[c.Code] = true
			gt.probeIDs = append(gt.probeIDs, states[c.Code].probeIDs...)
		}
	}

	// Phase 3: one measurement per distinct target; fetch + attribute to (country, provider).
	for _, id := range order {
		gt := targets[id]
		if rc.APIKey == "" || len(gt.probeIDs) == 0 {
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
			collectErrs = append(collectErrs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		if created {
			log.Printf("target %s: created measurement %d (%d probes, %d countries)", id, msmID, len(gt.probeIDs), len(gt.countries))
		} else {
			log.Printf("target %s: reusing measurement %d", id, msmID)
			// Reconcile probe roster to match today's config. New adds participate on
			// the next interval tick (RIPE has no immediate-trigger API).
			added, removed, err := rc.ReconcileProbes(ctx, msmID, gt.probeIDs)
			switch {
			case err != nil:
				log.Printf("target %s: reconcile FAILED (intended +%d -%d): %v", id, len(added), len(removed), err)
				collectErrs = append(collectErrs, fmt.Errorf("%s reconcile: %w", id, err))
			case len(added) > 0 || len(removed) > 0:
				log.Printf("target %s: reconciled msm %d: +%d (%v) / -%d (%v)", id, msmID, len(added), added, len(removed), removed)
			}
		}
		pings, err := rc.FetchResults(ctx, msmID, window)
		if err != nil {
			log.Printf("target %s: fetch: %v", id, err)
			collectErrs = append(collectErrs, fmt.Errorf("%s: %w", id, err))
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

	// Phase 4: score + export per country.
	var reps []score.CountryReport
	for _, c := range cfg.Countries {
		st := states[c.Code]
		rep := score.Compute(c, cfg.Thresholds, st.probeCounts, st.results, now)
		reps = append(reps, rep)
		log.Printf("scored %s: %d providers, %d samples in window", c.Code, len(rep.Providers), len(st.results))
	}

	for _, rep := range reps {
		if err := export.WriteCountry(*jsonDir, rep); err != nil {
			return err
		}
	}
	if err := export.WriteCountryList(*jsonDir, reps, now); err != nil {
		return err
	}
	log.Printf("exported JSON artifacts to %s", *jsonDir)

	// Surface collection failures as a non-zero exit (artifacts were still written).
	if len(collectErrs) > 0 {
		return fmt.Errorf("collection had errors:\n%w", errors.Join(collectErrs...))
	}
	return nil
}

// nonNeg clamps RIPE's -1 "no RTT" sentinel (fully-lost ping) to 0.
func nonNeg(f float64) float64 {
	if f < 0 {
		return 0
	}
	return f
}

// stopAll stops every active RIPE measurement created by packetloss (description
// prefix "packetloss "). Used by `make stop-measurements` to reset.
func stopAll(ctx context.Context, rc *ripe.Client) error {
	mine, err := rc.ListMine(ctx)
	if err != nil {
		return err
	}
	var stopped, failed int
	for _, m := range mine {
		if m.Status.ID > 2 || !strings.HasPrefix(m.Description, "packetloss ") {
			continue // already stopped, or not ours
		}
		if err := rc.StopMeasurement(ctx, m.ID); err != nil {
			log.Printf("stop %d (%s): %v", m.ID, m.Description, err)
			failed++
			continue
		}
		log.Printf("stopped %d (%s)", m.ID, m.Description)
		stopped++
	}
	log.Printf("stop-measurements: %d stopped, %d failed", stopped, failed)
	if failed > 0 {
		return fmt.Errorf("failed to stop %d measurement(s)", failed)
	}
	return nil
}
