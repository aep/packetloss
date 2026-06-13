// Package score turns windowed measurement history into a fully scored country
// report: relative 0..10 scores per (provider,target), absolute green/red status,
// downsampled chart series, and the last-30 drill-down. Pure functions, no I/O.
package score

import (
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

type Measurement struct {
	TS      time.Time
	RTTMin  float64
	RTTAvg  float64
	RTTMax  float64
	LossPct float64
	ProbeID uint32
}

type Cell struct {
	TargetID string
	Status   Status
	Score    float64
	RTTp50   float64
	LossPct  float64
	HasData  bool
}

type ProviderReport struct {
	ASN        uint32
	Name       string
	Score      float64
	Status     Status
	Covered    bool
	ProbeCount int
	Cells      []Cell
	Series     map[string][]TimePoint
	Last30     map[string][]Measurement
}

type TargetRef struct{ ID, Name, Kind string }

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
		rep.Targets = append(rep.Targets, TargetRef{ID: t.ID, Name: t.Name, Kind: t.Kind})
	}

	byPT := map[ptKey][]Result{}
	for _, r := range results {
		k := ptKey{r.ProviderASN, r.TargetID}
		byPT[k] = append(byPT[k], r)
	}

	type agg struct {
		rttP50, lossPct, cost float64
		hasData               bool
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
			aggs[ptKey{p.ASN, t.ID}] = agg{rttP50: p50, lossPct: loss, cost: p50 + loss*th.LossWeightK, hasData: true}
		}
	}

	// Per-target cost range over covered providers, for relative normalisation.
	type mm struct{ lo, hi float64 }
	ranges := map[string]mm{}
	for _, t := range c.Targets {
		first := true
		var r mm
		for _, p := range c.Providers {
			if probeCounts[p.ASN] < th.MinProbes {
				continue
			}
			a, ok := aggs[ptKey{p.ASN, t.ID}]
			if !ok {
				continue
			}
			if first {
				r = mm{a.cost, a.cost}
				first = false
				continue
			}
			if a.cost < r.lo {
				r.lo = a.cost
			}
			if a.cost > r.hi {
				r.hi = a.cost
			}
		}
		ranges[t.ID] = r
	}

	for _, p := range c.Providers {
		pr := ProviderReport{
			ASN:        p.ASN,
			Name:       p.Name,
			ProbeCount: probeCounts[p.ASN],
			Covered:    probeCounts[p.ASN] >= th.MinProbes,
			Series:     map[string][]TimePoint{},
			Last30:     map[string][]Measurement{},
		}
		var scoreSum float64
		var scored, red int
		worst := StatusGreen
		for _, t := range c.Targets {
			a, ok := aggs[ptKey{p.ASN, t.ID}]
			cell := Cell{TargetID: t.ID, HasData: ok, RTTp50: a.rttP50, LossPct: a.lossPct}
			switch {
			case !pr.Covered:
				cell.Status = StatusInsufficientCoverage
			case ok:
				cell.Score = relScore(a.cost, ranges[t.ID].lo, ranges[t.ID].hi)
				cell.Status = absStatus(a.rttP50, a.lossPct, th)
				scoreSum += cell.Score
				scored++
				if cell.Status == StatusRed {
					red++
				}
				if cell.Status > worst {
					worst = cell.Status
				}
			default:
				cell.Status = StatusUnspecified
			}
			pr.Cells = append(pr.Cells, cell)
			pr.Series[t.ID] = series(byPT[ptKey{p.ASN, t.ID}])
			pr.Last30[t.ID] = last30(byPT[ptKey{p.ASN, t.ID}])
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
			pr.Status = worst
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

// relScore maps cost to 0..10 relative to peers: best (lo) -> 10, worst (hi) -> 0.
func relScore(cost, lo, hi float64) float64 {
	if hi <= lo {
		return 10
	}
	s := 10 * (hi - cost) / (hi - lo)
	switch {
	case s < 0:
		s = 0
	case s > 10:
		s = 10
	}
	return round1(s)
}

func absStatus(rtt, loss float64, th config.Thresholds) Status {
	switch {
	case loss <= th.GreenMaxLossPct && rtt <= th.GreenMaxRTTMs:
		return StatusGreen
	case loss <= th.AmberMaxLossPct && rtt <= th.AmberMaxRTTMs:
		return StatusAmber
	default:
		return StatusRed
	}
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

// last30 returns the 30 most recent results, newest first.
func last30(rs []Result) []Measurement {
	out := make([]Measurement, 0, len(rs))
	for _, r := range rs {
		out = append(out, Measurement{
			TS: r.TS, RTTMin: r.RTTMin, RTTAvg: r.RTTAvg, RTTMax: r.RTTMax,
			LossPct: r.LossPct, ProbeID: r.ProbeID,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TS.After(out[j].TS) })
	if len(out) > 30 {
		out = out[:30]
	}
	return out
}

func round1(f float64) float64 { return float64(int(f*10+0.5)) / 10 }
