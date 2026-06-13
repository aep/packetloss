// Package export maps the scored report onto the protobuf contract and writes the
// JSON artifact tree (protojson) the web layer consumes.
package export

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	pb "github.com/exys/packetloss/builder/internal/pb/packetloss/v1"
	"github.com/exys/packetloss/builder/internal/score"
)

var marshal = protojson.MarshalOptions{Multiline: true, Indent: "  ", EmitDefaultValues: true}

// WriteCountry writes overview.json, providers/<asn>.json and targets/<id>.json.
// msmIDs maps target id -> RIPE Atlas measurement id, for the per-target source link.
func WriteCountry(jsonDir string, rep score.CountryReport, msmIDs map[string]int64) error {
	base := filepath.Join(jsonDir, rep.Code)
	gen := rep.GeneratedAt.Format(time.RFC3339)

	ov := &pb.Overview{CountryCode: rep.Code, CountryName: rep.Name, GeneratedAt: gen}
	for _, t := range rep.Targets {
		ov.Targets = append(ov.Targets, &pb.TargetRef{Id: t.ID, Name: t.Name, Kind: t.Kind})
	}
	for _, p := range rep.Providers {
		row := &pb.ProviderRow{
			Asn: p.ASN, Name: p.Name, Score: p.Score, Status: status(p.Status),
			Covered: p.Covered, ProbeCount: uint32(p.ProbeCount),
		}
		for _, c := range p.Cells {
			row.Cells = append(row.Cells, &pb.Cell{
				TargetId: c.TargetID, Status: status(c.Status), Score: c.Score,
				RttP50Ms: c.RTTp50, LossPct: c.LossPct, JitterMs: c.Jitter, HasData: c.HasData,
			})
		}
		ov.Providers = append(ov.Providers, row)
	}
	if err := writeMsg(filepath.Join(base, "overview.json"), ov); err != nil {
		return err
	}

	for _, p := range rep.Providers {
		d := &pb.ProviderDetail{
			CountryCode: rep.Code, Asn: p.ASN, Name: p.Name, Score: p.Score,
			Status: status(p.Status), Covered: p.Covered, ProbeCount: uint32(p.ProbeCount),
			GeneratedAt: gen,
		}
		for _, t := range rep.Targets {
			ts := &pb.TargetSeries{TargetId: t.ID, TargetName: t.Name, MeasurementId: uint32(msmIDs[t.ID])}
			for _, c := range p.Cells {
				if c.TargetID == t.ID {
					ts.Score, ts.Status = c.Score, status(c.Status)
				}
			}
			for _, tp := range p.Series[t.ID] {
				ts.Series = append(ts.Series, timePoint(tp))
			}
			d.Targets = append(d.Targets, ts)
		}
		if err := writeMsg(filepath.Join(base, "providers", fmt.Sprintf("%d.json", p.ASN)), d); err != nil {
			return err
		}
	}

	for _, t := range rep.Targets {
		tc := &pb.TargetComparison{
			CountryCode: rep.Code, TargetId: t.ID, TargetName: t.Name, Kind: t.Kind,
			MeasurementId: uint32(msmIDs[t.ID]), GeneratedAt: gen,
		}
		for _, p := range rep.Providers {
			if !p.Covered {
				continue
			}
			ps := &pb.ProviderSeries{Asn: p.ASN, Name: p.Name}
			for _, c := range p.Cells {
				if c.TargetID == t.ID {
					ps.Score, ps.Status = c.Score, status(c.Status)
				}
			}
			for _, tp := range p.Series[t.ID] {
				ps.Series = append(ps.Series, timePoint(tp))
			}
			tc.Providers = append(tc.Providers, ps)
		}
		if err := writeMsg(filepath.Join(base, "targets", fmt.Sprintf("%s.json", t.ID)), tc); err != nil {
			return err
		}
	}
	return nil
}

// WriteCountryList writes the top-level countries.json index.
func WriteCountryList(jsonDir string, reps []score.CountryReport, now time.Time) error {
	cl := &pb.CountryList{GeneratedAt: now.Format(time.RFC3339)}
	for _, r := range reps {
		cl.Countries = append(cl.Countries, &pb.CountrySummary{
			Code: r.Code, Name: r.Name,
			ProviderCount: uint32(len(r.Providers)), TargetCount: uint32(len(r.Targets)),
		})
	}
	return writeMsg(filepath.Join(jsonDir, "countries.json"), cl)
}

func timePoint(tp score.TimePoint) *pb.TimePoint {
	return &pb.TimePoint{Ts: tp.TS.Format(time.RFC3339), RttMs: tp.RTTms, LossPct: tp.LossPct}
}

func writeMsg(path string, m proto.Message) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := marshal.Marshal(m)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func status(s score.Status) pb.Status {
	switch s {
	case score.StatusGreen:
		return pb.Status_STATUS_GREEN
	case score.StatusAmber:
		return pb.Status_STATUS_AMBER
	case score.StatusRed:
		return pb.Status_STATUS_RED
	case score.StatusInsufficientCoverage:
		return pb.Status_STATUS_INSUFFICIENT_COVERAGE
	default:
		return pb.Status_STATUS_UNSPECIFIED
	}
}
