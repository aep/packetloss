// Package score turns windowed measurement history into a fully scored country
// report: absolute 0..10 scores per (provider,target) from a fixed latency+loss+jitter
// cost curve, green/red status derived from that score, and downsampled chart series.
// Scores are peer-independent and comparable across countries. Pure functions, no I/O.
package score

import (
	"math"
	"slices"
	"sort"
	"time"

	"github.com/exys/packetloss/builder/internal/config"
)

// Result is one ping aggregate fed into scoring (one probe, one window). Built from
// RIPE Atlas results by the collector — there is no local DB.
type Result struct {
	ProviderASN uint32
	TargetID    string
	ProbeID     uint32
	TS          time.Time
	RTTMin      float64
	RTTAvg      float64
	RTTMax      float64
	LossPct     float64
}

type Status int

const (
	StatusUnspecified Status = iota
	StatusGreen
	StatusAmber
	StatusRed
	StatusInsufficientCoverage
)

type TimePoint struct {
	TS      time.Time
	RTTms   float64
	LossPct float64
}

type Cell struct {
	TargetID string
	Status   Status
	Score    float64
	RTTp50   float64
	LossPct  float64
	Jitter   float64
	HasData  bool
}

type ProviderReport struct {
	ASN        uint32
	Name       string
	Kind       string
	Score      float64
	Status     Status
	Covered    bool
	ProbeCount int
	ProbeIDs   []uint32 // distinct RIPE probe IDs (inside this ASN) that returned data, sorted
	Cells      []Cell
	Series     map[string][]TimePoint
}

type TargetRef struct {
	ID, Name, Kind string
	Target         string // RIPE ping target: hostname or pinned IP
	ResolveOnProbe bool   // hostname resolved per-probe (anycast/CDN nearest edge)
}

type CountryReport struct {
	Code        string
	Name        string
	GeneratedAt time.Time
	Targets     []TargetRef
	Providers   []ProviderReport
}

type ptKey struct {
	asn uint32
	tid string
}

// Compute scores one country. probeCounts gates coverage; results is the window.
func Compute(c config.Country, th config.Thresholds, probeCounts map[uint32]int, results []Result, now time.Time) CountryReport {
	rep := CountryReport{Code: c.Code, Name: c.Name, GeneratedAt: now}
	for _, t := range c.Targets {
		endpoint, resolveOnProbe := t.Endpoint()
		rep.Targets = append(rep.Targets, TargetRef{
			ID: t.ID, Name: t.Name, Kind: t.Kind,
			Target: endpoint, ResolveOnProbe: resolveOnProbe,
		})
	}

	byPT := map[ptKey][]Result{}
	probesByASN := map[uint32]map[uint32]bool{}
	for _, r := range results {
		k := ptKey{r.ProviderASN, r.TargetID}
		byPT[k] = append(byPT[k], r)
		if probesByASN[r.ProviderASN] == nil {
			probesByASN[r.ProviderASN] = map[uint32]bool{}
		}
		probesByASN[r.ProviderASN][r.ProbeID] = true
	}

	type agg struct {
		rttP50, lossPct, jitter, cost float64
		hasData                       bool
	}
	aggs := map[ptKey]agg{}
	for _, p := range c.Providers {
		for _, t := range c.Targets {
			rs := byPT[ptKey{p.ASN, t.ID}]
			if len(rs) == 0 {
				continue
			}
			rtts := make([]float64, 0, len(rs))
			var lossSum float64
			for _, r := range rs {
				if r.RTTAvg > 0 {
					rtts = append(rtts, r.RTTAvg)
				}
				lossSum += r.LossPct
			}
			p50 := median(rtts)
			loss := lossSum / float64(len(rs))
			jit := jitter(rs)
			cost := p50 + loss*th.LossWeightK + jit*th.JitterWeightK
			aggs[ptKey{p.ASN, t.ID}] = agg{rttP50: p50, lossPct: loss, jitter: jit, cost: cost, hasData: true}
		}
	}

	for _, p := range c.Providers {
		pr := ProviderReport{
			ASN:        p.ASN,
			Name:       p.Name,
			Kind:       p.Kind,
			ProbeCount: probeCounts[p.ASN],
			ProbeIDs:   sortedProbeIDs(probesByASN[p.ASN]),
			Covered:    probeCounts[p.ASN] >= th.MinProbes,
			Series:     map[string][]TimePoint{},
		}
		var scoreSum float64
		var scored, red int
		for _, t := range c.Targets {
			a, ok := aggs[ptKey{p.ASN, t.ID}]
			cell := Cell{TargetID: t.ID, HasData: ok, RTTp50: a.rttP50, LossPct: a.lossPct, Jitter: a.jitter}
			switch {
			case !pr.Covered:
				cell.Status = StatusInsufficientCoverage
			case ok:
				cell.Score = absScore(a.cost, th)
				cell.Status = statusFromScore(cell.Score, th)
				scoreSum += cell.Score
				scored++
				if cell.Status == StatusRed {
					red++
				}
			default:
				cell.Status = StatusUnspecified
			}
			pr.Cells = append(pr.Cells, cell)
			pr.Series[t.ID] = series(byPT[ptKey{p.ASN, t.ID}])
		}
		switch {
		case !pr.Covered:
			pr.Status = StatusInsufficientCoverage
		case scored > 0:
			// Mean cell score, then reduced by the share of red cells: one
			// destination unreachable out of six knocks ~1/6 off the score.
			mean := scoreSum / float64(scored)
			reliability := float64(scored-red) / float64(scored)
			pr.Score = round1(mean * reliability)
			// Colour the provider from its overall score, same scale as the cells,
			// so the badge can't disagree with the number it sits next to.
			pr.Status = statusFromScore(pr.Score, th)
		}
		rep.Providers = append(rep.Providers, pr)
	}

	// Best first: covered before uncovered, then score desc.
	sort.SliceStable(rep.Providers, func(i, j int) bool {
		a, b := rep.Providers[i], rep.Providers[j]
		if a.Covered != b.Covered {
			return a.Covered
		}
		return a.Score > b.Score
	})
	return rep
}

// absScore maps cost to an absolute 0..10 on a fixed exponential-decay curve,
// independent of peers: a full 10 requires cost <= CostAtScore10 (near-perfect:
// ~1ms, no loss, no jitter), and every extra ms of cost decays the score by
// exp(-1/ScoreDecayMs). Concave at the top so excellent paths spread out instead of
// all pinning at 10; peer-independent and comparable across countries.
func absScore(cost float64, th config.Thresholds) float64 {
	if th.ScoreDecayMs <= 0 || cost <= th.CostAtScore10 {
		return 10
	}
	s := 10 * math.Exp(-(cost-th.CostAtScore10)/th.ScoreDecayMs)
	switch {
	case s < 0:
		s = 0
	case s > 10:
		s = 10
	}
	return round1(s)
}

// statusFromScore derives the green/amber/red grid colour from the absolute score.
func statusFromScore(score float64, th config.Thresholds) Status {
	switch {
	case score >= th.GreenMinScore:
		return StatusGreen
	case score >= th.AmberMinScore:
		return StatusAmber
	default:
		return StatusRed
	}
}

// jitter is the RFC3550 / ITU-T-style packet delay variation: the mean absolute
// difference between consecutive RTT samples in time order. A flat line scores ~0;
// occasional spikes (up then back down) each contribute two large deltas. Only
// valid (>0) RTTAvg samples are used.
func jitter(rs []Result) float64 {
	ord := slices.Clone(rs)
	slices.SortFunc(ord, func(a, b Result) int { return a.TS.Compare(b.TS) })
	var prev float64
	var sum float64
	var n int
	have := false
	for _, r := range ord {
		if r.RTTAvg <= 0 {
			continue
		}
		if have {
			d := r.RTTAvg - prev
			if d < 0 {
				d = -d
			}
			sum += d
			n++
		}
		prev = r.RTTAvg
		have = true
	}
	if n == 0 {
		return 0
	}
	return sum / float64(n)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// series downsamples raw results into hourly buckets (median RTT, mean loss).
func series(rs []Result) []TimePoint {
	if len(rs) == 0 {
		return nil
	}
	type bucket struct {
		rtts []float64
		loss float64
		n    int
	}
	buckets := map[int64]*bucket{}
	var hours []int64
	for _, r := range rs {
		h := r.TS.Truncate(time.Hour).Unix()
		b := buckets[h]
		if b == nil {
			b = &bucket{}
			buckets[h] = b
			hours = append(hours, h)
		}
		if r.RTTAvg > 0 {
			b.rtts = append(b.rtts, r.RTTAvg)
		}
		b.loss += r.LossPct
		b.n++
	}
	slices.Sort(hours)
	out := make([]TimePoint, 0, len(hours))
	for _, h := range hours {
		b := buckets[h]
		out = append(out, TimePoint{
			TS:      time.Unix(h, 0).UTC(),
			RTTms:   round1(median(b.rtts)),
			LossPct: round1(b.loss / float64(b.n)),
		})
	}
	return out
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }

// sortedProbeIDs flattens a probe-ID set into an ascending slice (nil stays nil so
// uncovered providers export no probe links).
func sortedProbeIDs(set map[uint32]bool) []uint32 {
	if len(set) == 0 {
		return nil
	}
	ids := make([]uint32, 0, len(set))
	for id := range set {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}
