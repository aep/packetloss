package score

import (
	"math"
	"testing"
	"time"

	"github.com/exys/packetloss/builder/internal/config"
)

func testThresholds() config.Thresholds {
	return config.Thresholds{
		WindowDays:    7,
		LossWeightK:   10,
		JitterWeightK: 2,
		MinProbes:     1,
		CostAtScore10: 1,
		ScoreDecayMs:  80,
		GreenMinScore: 7,
		AmberMinScore: 3,
	}
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.05 }

func TestJitter(t *testing.T) {
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	// Flat line: no variation -> 0 jitter.
	flat := []Result{
		{TS: base, RTTAvg: 20}, {TS: base.Add(time.Hour), RTTAvg: 20}, {TS: base.Add(2 * time.Hour), RTTAvg: 20},
	}
	if g := jitter(flat); !approx(g, 0) {
		t.Fatalf("flat jitter = %v, want 0", g)
	}
	// One spike up then back down: deltas 0,40,40 over 3 transitions -> 80/3.
	spiky := []Result{
		{TS: base, RTTAvg: 20}, {TS: base.Add(time.Hour), RTTAvg: 20},
		{TS: base.Add(2 * time.Hour), RTTAvg: 60}, {TS: base.Add(3 * time.Hour), RTTAvg: 20},
	}
	if g := jitter(spiky); !approx(g, 80.0/3) {
		t.Fatalf("spiky jitter = %v, want %v", g, 80.0/3)
	}
	// Out-of-order input must be sorted by TS first.
	unordered := []Result{
		{TS: base.Add(3 * time.Hour), RTTAvg: 20}, {TS: base, RTTAvg: 20},
		{TS: base.Add(time.Hour), RTTAvg: 20}, {TS: base.Add(2 * time.Hour), RTTAvg: 60},
	}
	if g := jitter(unordered); !approx(g, 80.0/3) {
		t.Fatalf("unordered jitter = %v, want %v", g, 80.0/3)
	}
}

func TestAbsScoreAndStatus(t *testing.T) {
	th := testThresholds()
	// score = 10*exp(-(cost-1)/80), rounded to 0.1.
	cases := []struct {
		cost   float64
		score  float64
		status Status
	}{
		{cost: 1, score: 10, status: StatusGreen},    // perfect: <=1ms, no loss, no jitter
		{cost: 0.5, score: 10, status: StatusGreen},  // even better, clamped at 10
		{cost: 10, score: 8.9, status: StatusGreen},  // 10*exp(-9/80)
		{cost: 29.5, score: 7.0, status: StatusGreen}, // ~green boundary
		{cost: 97.3, score: 3.0, status: StatusAmber}, // ~amber boundary
		{cost: 500, score: 0, status: StatusRed},      // far tail rounds to 0
	}
	for _, c := range cases {
		got := absScore(c.cost, th)
		if !approx(got, c.score) {
			t.Errorf("absScore(%v) = %v, want %v", c.cost, got, c.score)
		}
		if st := statusFromScore(got, th); st != c.status {
			t.Errorf("statusFromScore(score %v) = %v, want %v", got, st, c.status)
		}
	}
}

// TestJitterFlipsRanking reproduces the Cogent-vs-euNetworks case: a spiky provider
// with a lower median must now rank below a flat, slightly-higher-median one.
func TestJitterFlipsRanking(t *testing.T) {
	th := testThresholds()
	base := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	c := config.Country{
		Code: "de", Name: "Germany",
		Providers: []config.Provider{{ASN: 1, Name: "flat"}, {ASN: 2, Name: "spiky"}},
		Targets:   []config.Target{{ID: "aws", Name: "AWS", Host: "aws.amazon.com"}},
	}
	var results []Result
	for i := 0; i < 12; i++ {
		ts := base.Add(time.Duration(i) * time.Hour)
		// flat: steady 22ms
		results = append(results, Result{ProviderASN: 1, TargetID: "aws", TS: ts, RTTAvg: 22})
		// spiky: 15ms median, spikes to 70ms every 3rd sample
		rtt := 15.0
		if i%3 == 0 {
			rtt = 70
		}
		results = append(results, Result{ProviderASN: 2, TargetID: "aws", TS: ts, RTTAvg: rtt})
	}
	probes := map[uint32]int{1: 5, 2: 5}
	rep := Compute(c, th, probes, results, base.Add(13*time.Hour))

	if len(rep.Providers) != 2 {
		t.Fatalf("got %d providers", len(rep.Providers))
	}
	// Sorted best-first: flat must come first despite spiky's lower median.
	if rep.Providers[0].Name != "flat" {
		t.Fatalf("ranking not flipped: got %q first (scores: flat? spiky?)", rep.Providers[0].Name)
	}
	if rep.Providers[0].Score <= rep.Providers[1].Score {
		t.Fatalf("flat score %.1f should beat spiky %.1f", rep.Providers[0].Score, rep.Providers[1].Score)
	}
}
