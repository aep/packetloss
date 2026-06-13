// Build-time loaders for the builder's protojson artifacts. The web layer reads
// ONLY these JSON files (DATA_DIR) — never SQLite. Each file is validated against
// the generated protobuf schema via fromJson.
import { readFileSync, existsSync } from 'node:fs';
import { resolve } from 'node:path';
import { fromJson, type JsonValue } from '@bufbuild/protobuf';
import {
  CountryListSchema, type CountryList,
  OverviewSchema, type Overview,
  ProviderDetailSchema, type ProviderDetail,
  TargetComparisonSchema, type TargetComparison,
} from '../gen/packetloss/v1/artifacts_pb';

const DATA_DIR = process.env.DATA_DIR ?? resolve(process.cwd(), '../data/json');

function read(...parts: string[]): JsonValue {
  return JSON.parse(readFileSync(resolve(DATA_DIR, ...parts), 'utf8')) as JsonValue;
}

export function getCountries(): CountryList {
  return fromJson(CountryListSchema, read('countries.json'));
}

export function getOverview(code: string): Overview {
  return fromJson(OverviewSchema, read(code, 'overview.json'));
}

export function getProviderDetail(code: string, asn: number | string): ProviderDetail {
  return fromJson(ProviderDetailSchema, read(code, 'providers', `${asn}.json`));
}

export function getTargetComparison(code: string, id: string): TargetComparison {
  return fromJson(TargetComparisonSchema, read(code, 'targets', `${id}.json`));
}

export function hasProviderDetail(code: string, asn: number | string): boolean {
  return existsSync(resolve(DATA_DIR, code, 'providers', `${asn}.json`));
}

export function hasTargetComparison(code: string, id: string): boolean {
  return existsSync(resolve(DATA_DIR, code, 'targets', `${id}.json`));
}

// Shared y-axis bounds across every sparkline so 0 sits at the same height and equal
// values map to equal heights in all charts (comparable across providers, targets and
// pages). Scans every provider-detail series — the superset of all charted data —
// once, then caches.
let _yBounds: { rtt: number; loss: number } | null = null;
export function getYBounds(): { rtt: number; loss: number } {
  if (_yBounds) return _yBounds;
  let rtt = 1;
  let loss = 1;
  for (const c of getCountries().countries) {
    for (const p of getOverview(c.code).providers) {
      if (!hasProviderDetail(c.code, p.asn)) continue;
      for (const t of getProviderDetail(c.code, p.asn).targets) {
        for (const pt of t.series) {
          if (pt.rttMs > rtt) rtt = pt.rttMs;
          if (pt.lossPct > loss) loss = pt.lossPct;
        }
      }
    }
  }
  _yBounds = { rtt, loss };
  return _yBounds;
}
