// Package ripe is a thin typed client over the RIPE Atlas REST API. Only the
// surface the builder needs: list probes by ASN, ensure ping measurements, fetch
// results. The API is plain JSON over HTTPS, so net/http suffices.
package ripe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const defaultBase = "https://atlas.ripe.net/api/v2"

type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client
}

func New(apiKey string) *Client {
	return &Client{BaseURL: defaultBase, APIKey: apiKey, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

type Probe struct {
	ID          uint32 `json:"id"`
	ASNv4       uint32 `json:"asn_v4"`
	CountryCode string `json:"country_code"`
	Status      struct {
		Name string `json:"name"`
	} `json:"status"`
}

// ProbesByASN returns up to `limit` connected probes whose IPv4 origin AS is asn,
// plus `total`, the full count of connected probes in that AS (for coverage gating —
// we display/gate on the true total but only measure from the capped sample).
func (c *Client) ProbesByASN(ctx context.Context, asn uint32, country string, limit int) (probes []Probe, total int, err error) {
	q := url.Values{}
	q.Set("asn_v4", strconv.FormatUint(uint64(asn), 10))
	q.Set("status", "1") // 1 = Connected
	if country != "" {
		q.Set("country_code", country)
	}
	if limit > 0 {
		q.Set("page_size", strconv.Itoa(limit))
	}
	var resp struct {
		Count   int     `json:"count"`
		Results []Probe `json:"results"`
	}
	if err := c.getJSON(ctx, "/probes/?"+q.Encode(), &resp); err != nil {
		return nil, 0, err
	}
	return resp.Results, resp.Count, nil
}

// PingResult is one row of a ping measurement's results.
type PingResult struct {
	PrbID     uint32  `json:"prb_id"`
	Timestamp int64   `json:"timestamp"`
	Sent      int     `json:"sent"`
	Rcvd      int     `json:"rcvd"`
	Min       float64 `json:"min"`
	Avg       float64 `json:"avg"`
	Max       float64 `json:"max"`
}

// LossPct derives packet loss from sent/received.
func (p PingResult) LossPct() float64 {
	if p.Sent == 0 {
		return 100
	}
	return float64(p.Sent-p.Rcvd) / float64(p.Sent) * 100
}

// FetchResults returns ping results for a measurement at or after `start`.
func (c *Client) FetchResults(ctx context.Context, msmID int64, start time.Time) ([]PingResult, error) {
	q := url.Values{}
	q.Set("format", "json")
	if !start.IsZero() {
		q.Set("start", strconv.FormatInt(start.Unix(), 10))
	}
	var out []PingResult
	path := fmt.Sprintf("/measurements/%d/results/?%s", msmID, q.Encode())
	if err := c.getJSON(ctx, path, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type pingDefinition struct {
	Type           string `json:"type"`
	AF             int    `json:"af"`
	Target         string `json:"target"`
	Description    string `json:"description"`
	Interval       int    `json:"interval"`
	Packets        int    `json:"packets"`
	IsOneoff       bool   `json:"is_oneoff"`
	ResolveOnProbe bool   `json:"resolve_on_probe"`
}

type probeRequest struct {
	Type      string `json:"type"`
	Value     string `json:"value"`
	Requested int    `json:"requested"`
}

type createRequest struct {
	Definitions []pingDefinition `json:"definitions"`
	Probes      []probeRequest   `json:"probes"`
}

// EnsureMeasurement returns the id of an ongoing ping measurement with the given
// description, reusing one we already own if present (idempotent against RIPE state),
// otherwise creating it. `created` reports which happened. target is an IP
// (resolveOnProbe=false) or a hostname resolved per-probe (resolveOnProbe=true).
// Requires APIKey.
func (c *Client) EnsureMeasurement(ctx context.Context, target, description string, resolveOnProbe bool, probeIDs []uint32, intervalSec, packets int) (id int64, created bool, err error) {
	if c.APIKey == "" {
		return 0, false, fmt.Errorf("ripe: API key required to create measurements")
	}
	if existing, found, err := c.findOngoing(ctx, description); err != nil {
		return 0, false, err
	} else if found {
		return existing, false, nil
	}

	af := 4
	if ip := net.ParseIP(target); ip != nil && ip.To4() == nil {
		af = 6
	}
	ids := make([]string, len(probeIDs))
	for i, id := range probeIDs {
		ids[i] = strconv.FormatUint(uint64(id), 10)
	}
	body := createRequest{
		Definitions: []pingDefinition{{
			Type: "ping", AF: af, Target: target, Description: description,
			Interval: intervalSec, Packets: packets, IsOneoff: false, ResolveOnProbe: resolveOnProbe,
		}},
		Probes: []probeRequest{{Type: "probes", Value: strings.Join(ids, ","), Requested: len(ids)}},
	}
	var resp struct {
		Measurements []int64 `json:"measurements"`
	}
	if err := c.postJSON(ctx, "/measurements/", body, &resp); err != nil {
		return 0, false, err
	}
	if len(resp.Measurements) == 0 {
		return 0, false, fmt.Errorf("ripe: create returned no measurement id")
	}
	return resp.Measurements[0], true, nil
}

// MeasurementSummary is a row from the measurement list.
type MeasurementSummary struct {
	ID          int64  `json:"id"`
	Description string `json:"description"`
	Status      struct {
		ID int `json:"id"` // 0 specified, 1 scheduled, 2 ongoing, 4+ stopped
	} `json:"status"`
}

func (m MeasurementSummary) active() bool { return m.Status.ID <= 2 }

// ListMine returns the caller's measurements (any status), following pagination.
// Uses the /measurements/my/ endpoint — the ?mine=true filter is silently ignored
// and returns the entire public list instead.
func (c *Client) ListMine(ctx context.Context) ([]MeasurementSummary, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("ripe: API key required to list measurements")
	}
	var out []MeasurementSummary
	next := "/measurements/my/?page_size=500"
	for next != "" {
		var resp struct {
			Next    string               `json:"next"`
			Results []MeasurementSummary `json:"results"`
		}
		if err := c.getJSON(ctx, next, &resp); err != nil {
			return nil, err
		}
		out = append(out, resp.Results...)
		next = resp.Next
	}
	return out, nil
}

// findOngoing returns the id of an active measurement we own with an exactly matching
// description, if any.
func (c *Client) findOngoing(ctx context.Context, description string) (int64, bool, error) {
	mine, err := c.ListMine(ctx)
	if err != nil {
		return 0, false, err
	}
	for _, m := range mine {
		if m.active() && m.Description == description {
			return m.ID, true, nil
		}
	}
	return 0, false, nil
}

// participationRequest is a row from /measurements/{id}/participation-requests/.
// Each row records an add or remove action on a set of probes (or ASN/country/area
// targets — we only consume type="probes" rows).
type participationRequest struct {
	Action string `json:"action"` // "add" | "remove"
	Type   string `json:"type"`   // "probes" | "asn" | "country" | "area"
	Value  string `json:"value"`  // comma-separated probe IDs when Type=="probes"
}

// currentProbes derives the present probe roster of a measurement by replaying
// its participation-request history: sum explicit-probe adds, subtract removes.
// Probes RIPE silently rejected (offline at the time, cap exceeded) still appear
// here — RIPE's API doesn't expose the post-validation roster, so we treat the
// history as ground truth and let downstream "add already-present probe" calls
// no-op against duplicates. Rows with non-probe types are ignored.
func (c *Client) currentProbes(ctx context.Context, msmID int64) (map[uint32]bool, error) {
	if c.APIKey == "" {
		return nil, fmt.Errorf("ripe: API key required")
	}
	path := fmt.Sprintf("/measurements/%d/participation-requests/?page_size=500", msmID)
	set := map[uint32]bool{}
	for path != "" {
		var resp struct {
			Next    string                 `json:"next"`
			Results []participationRequest `json:"results"`
		}
		if err := c.getJSON(ctx, path, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Results {
			if r.Type != "probes" {
				continue
			}
			for _, s := range strings.Split(r.Value, ",") {
				s = strings.TrimSpace(s)
				if s == "" {
					continue
				}
				id64, err := strconv.ParseUint(s, 10, 32)
				if err != nil {
					continue
				}
				id := uint32(id64)
				switch r.Action {
				case "add":
					set[id] = true
				case "remove":
					delete(set, id)
				}
			}
		}
		path = resp.Next
	}
	return set, nil
}

// ReconcileProbes makes the measurement's probe roster match `desired` by POSTing
// add/remove participation-requests for the diff. Returns the IDs added and removed
// (sorted). Probes RIPE rejects on add (offline, capped, disallowed) surface as a
// non-nil error; the caller can still proceed since other targets are independent.
// New adds take effect on the next measurement interval (no immediate ping).
func (c *Client) ReconcileProbes(ctx context.Context, msmID int64, desired []uint32) (added, removed []uint32, err error) {
	if c.APIKey == "" {
		return nil, nil, fmt.Errorf("ripe: API key required")
	}
	current, err := c.currentProbes(ctx, msmID)
	if err != nil {
		return nil, nil, fmt.Errorf("fetch current probes: %w", err)
	}
	want := map[uint32]bool{}
	for _, id := range desired {
		want[id] = true
	}
	for id := range want {
		if !current[id] {
			added = append(added, id)
		}
	}
	for id := range current {
		if !want[id] {
			removed = append(removed, id)
		}
	}
	sort.Slice(added, func(i, j int) bool { return added[i] < added[j] })
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })

	var errs []error
	if len(added) > 0 {
		if e := c.postParticipation(ctx, msmID, "add", added); e != nil {
			errs = append(errs, fmt.Errorf("add: %w", e))
		}
	}
	if len(removed) > 0 {
		if e := c.postParticipation(ctx, msmID, "remove", removed); e != nil {
			errs = append(errs, fmt.Errorf("remove: %w", e))
		}
	}
	if len(errs) > 0 {
		return added, removed, errors.Join(errs...)
	}
	return added, removed, nil
}

func (c *Client) postParticipation(ctx context.Context, msmID int64, action string, ids []uint32) error {
	strs := make([]string, len(ids))
	for i, id := range ids {
		strs[i] = strconv.FormatUint(uint64(id), 10)
	}
	// RIPE expects an array of request objects (not a single object).
	body := []map[string]any{{
		"action":    action,
		"type":      "probes",
		"value":     strings.Join(strs, ","),
		"requested": len(ids),
	}}
	var resp json.RawMessage
	return c.postJSON(ctx, fmt.Sprintf("/measurements/%d/participation-requests/", msmID), body, &resp)
}

// StopMeasurement stops an ongoing measurement (DELETE).
func (c *Client) StopMeasurement(ctx context.Context, id int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, fmt.Sprintf("%s/measurements/%d/", c.BaseURL, id), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Key "+c.APIKey)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusAccepted {
		slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("ripe DELETE measurement %d: %s: %s", id, resp.Status, slurp)
	}
	return nil
}

// getJSON does a GET and decodes JSON, retrying transient failures (network errors,
// truncated bodies / "unexpected EOF", and 5xx). 4xx are returned immediately.
func (c *Client) getJSON(ctx context.Context, path string, v any) error {
	url := path
	if !strings.HasPrefix(url, "http") {
		url = c.BaseURL + path // else path is an absolute "next" pagination URL
	}
	const attempts = 3
	var lastErr error
	for attempt := range attempts {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		if c.APIKey != "" {
			req.Header.Set("Authorization", "Key "+c.APIKey)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err // network error — retry
			continue
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err // body truncated mid-stream (unexpected EOF) — retry
			continue
		}
		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode >= 500 {
				lastErr = fmt.Errorf("%s", resp.Status) // server error — retry
				continue
			}
			return fmt.Errorf("ripe GET %s: %s: %s", path, resp.Status, truncate(body))
		}
		if err := json.Unmarshal(body, v); err != nil {
			lastErr = fmt.Errorf("decode: %w", err) // garbled/truncated JSON — retry
			continue
		}
		return nil
	}
	return fmt.Errorf("ripe GET %s: %w", path, lastErr)
}

func truncate(b []byte) string {
	if len(b) > 512 {
		b = b[:512]
	}
	return string(b)
}

func (c *Client) postJSON(ctx context.Context, path string, body, v any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Key "+c.APIKey)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		slurp, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("ripe POST %s: %s: %s", path, resp.Status, slurp)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}
