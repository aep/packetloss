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
