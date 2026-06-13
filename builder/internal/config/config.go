// Package config loads and validates the YAML config: thresholds.yaml plus one
// <country>.yaml per country.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Thresholds struct {
	WindowDays                int     `yaml:"window_days"`
	LossWeightK               float64 `yaml:"loss_weight_k"`
	MinProbes                 int     `yaml:"min_probes"`
	ProbesPerProvider         int     `yaml:"probes_per_provider"`
	MeasurementIntervalSecond int     `yaml:"measurement_interval_seconds"`
	Packets                   int     `yaml:"packets"`
	GreenMaxLossPct           float64 `yaml:"green_max_loss_pct"`
	GreenMaxRTTMs             float64 `yaml:"green_max_rtt_ms"`
	AmberMaxLossPct           float64 `yaml:"amber_max_loss_pct"`
	AmberMaxRTTMs             float64 `yaml:"amber_max_rtt_ms"`
}

type Provider struct {
	ASN  uint32 `yaml:"asn"`
	Name string `yaml:"name"`
	// ProbeIDs, if set, overrides ASN-based probe resolution: the listed probes are
	// used directly (across any ASN/country) and attributed to this provider. Use
	// sparingly — e.g. a provider with no in-ASN probes but a customer that is
	// effectively single-homed behind it. Bypasses the probe-in-ASN attribution model.
	ProbeIDs []uint32 `yaml:"probe_ids,omitempty"`
}

// Target is a destination to measure. Set exactly one of:
//   - Address: a pinned IP (e.g. anycast DNS 8.8.8.8) — measured as-is.
//   - Host:    a hostname — RIPE resolves it per-probe at runtime (resolve_on_probe),
//     so each probe pings the edge its own resolver returns. Best for CDN/cloud.
type Target struct {
	ID      string `yaml:"id"`
	Name    string `yaml:"name"`
	Kind    string `yaml:"kind"`
	Address string `yaml:"address,omitempty"`
	Host    string `yaml:"host,omitempty"`
	ASN     uint32 `yaml:"asn,omitempty"`
}

// Endpoint returns the RIPE measurement target and whether to resolve per-probe.
func (t Target) Endpoint() (target string, resolveOnProbe bool) {
	if t.Host != "" {
		return t.Host, true
	}
	return t.Address, false
}

type Country struct {
	Code      string     `yaml:"code"`
	Name      string     `yaml:"name"`
	Providers []Provider `yaml:"providers"`
	Targets   []Target   `yaml:"targets"`
}

type Config struct {
	Thresholds Thresholds
	Countries  []Country
}

// Load reads thresholds.yaml and every other *.yaml in dir (one per country).
func Load(dir string) (*Config, error) {
	tb, err := os.ReadFile(filepath.Join(dir, "thresholds.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read thresholds: %w", err)
	}
	var th Thresholds
	if err := yaml.Unmarshal(tb, &th); err != nil {
		return nil, fmt.Errorf("parse thresholds: %w", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cfg := &Config{Thresholds: th}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || name == "thresholds.yaml" || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		var c Country
		if err := yaml.Unmarshal(b, &c); err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}
		cfg.Countries = append(cfg.Countries, c)
	}
	sort.Slice(cfg.Countries, func(i, j int) bool { return cfg.Countries[i].Code < cfg.Countries[j].Code })
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	if c.Thresholds.WindowDays <= 0 {
		return fmt.Errorf("thresholds.window_days must be > 0")
	}
	if c.Thresholds.MinProbes <= 0 {
		return fmt.Errorf("thresholds.min_probes must be > 0")
	}
	if len(c.Countries) == 0 {
		return fmt.Errorf("no country config files found in config dir")
	}
	for _, ct := range c.Countries {
		if ct.Code == "" || ct.Name == "" {
			return fmt.Errorf("country missing code/name")
		}
		if len(ct.Providers) == 0 || len(ct.Targets) == 0 {
			return fmt.Errorf("country %s needs providers and targets", ct.Code)
		}
		seen := map[string]bool{}
		for _, t := range ct.Targets {
			if t.ID == "" {
				return fmt.Errorf("country %s: target missing id", ct.Code)
			}
			if (t.Address == "") == (t.Host == "") {
				return fmt.Errorf("country %s target %q: set exactly one of address/host", ct.Code, t.ID)
			}
			if seen[t.ID] {
				return fmt.Errorf("country %s: duplicate target id %q", ct.Code, t.ID)
			}
			seen[t.ID] = true
		}
	}
	return nil
}
