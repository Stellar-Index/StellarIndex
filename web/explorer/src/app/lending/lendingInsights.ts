import { formatBaseUnits } from '@/lib/format';
import { scaledUnits } from '../explorer-shared';
import { fixedPct, isUnlimitedCap, supplyCapText } from './poolConfig';

export type HistoryMetric =
  | 'supply_apr'
  | 'borrow_apr'
  | 'utilization'
  | 'supplied'
  | 'borrowed'
  | 'collateral_factor'
  | 'liability_factor'
  | 'supply_cap';

/** A metric choice in the history picker; `aprs` overlays supply and borrow APR. */
export type HistoryView = HistoryMetric | 'aprs';

export interface HistoryPoint {
  t: string;
  removed: boolean;
  supplied: string | null;
  borrowed: string | null;
  utilization_pct: number | null;
  borrow_apr: number | null;
  supply_apr: number | null;
  collateral_factor: string | null;
  liability_factor: string | null;
  supply_cap: string | null;
}

export interface StepPoint {
  time: number;
  value: number | null;
}

export interface RangeOption {
  id: '7d' | '30d' | '90d' | '1y';
  label: string;
  window: string;
  interval: '1h' | '1d';
}

export const RANGES: RangeOption[] = [
  { id: '30d', label: '30 days', window: '30d', interval: '1d' },
  { id: '90d', label: '90 days', window: '90d', interval: '1d' },
  { id: '1y', label: '1 year', window: '365d', interval: '1d' },
  { id: '7d', label: '7 days (hourly)', window: '7d', interval: '1h' },
];

export const METRICS: { id: HistoryView; label: string; isPct: boolean }[] = [
  { id: 'aprs', label: 'Supply vs borrow APR', isPct: true },
  { id: 'supply_apr', label: 'Supply APR', isPct: true },
  { id: 'borrow_apr', label: 'Borrow APR', isPct: true },
  { id: 'utilization', label: 'Utilisation', isPct: true },
  { id: 'supplied', label: 'Supplied', isPct: false },
  { id: 'borrowed', label: 'Borrowed', isPct: false },
  { id: 'collateral_factor', label: 'Collateral factor', isPct: true },
  { id: 'liability_factor', label: 'Liability factor', isPct: true },
  { id: 'supply_cap', label: 'Supply cap', isPct: false },
];

const BUCKET_SECONDS = { '1h': 3600, '1d': 86_400 } as const;
const MAX_BUCKETS = 5000;

/** A metric's value at one point, or null when absent (never a fake 0). */
export function metricValue(
  p: HistoryPoint,
  metric: HistoryMetric,
  decimals: number | null | undefined,
  configDecimals?: number | null,
): number | null {
  if (p.removed) return null;
  switch (metric) {
    case 'collateral_factor':
    case 'liability_factor': {
      const raw = p[metric];
      if (raw == null || configDecimals == null) return null;
      const n = scaledUnits(raw, configDecimals);
      return Number.isFinite(n) ? n * 100 : null;
    }
    case 'supply_cap': {
      // A max placeholder is "no cap": a gap, never a giant spike.
      if (
        p.supply_cap == null ||
        decimals == null ||
        isUnlimitedCap(p.supply_cap)
      )
        return null;
      const n = scaledUnits(p.supply_cap, decimals);
      return Number.isFinite(n) ? n : null;
    }
    case 'supply_apr':
      return p.supply_apr == null ? null : p.supply_apr * 100;
    case 'borrow_apr':
      return p.borrow_apr == null ? null : p.borrow_apr * 100;
    case 'utilization':
      return p.utilization_pct;
    case 'supplied':
    case 'borrowed': {
      const raw = metric === 'supplied' ? p.supplied : p.borrowed;
      if (raw == null || decimals == null) return null;
      const n = scaledUnits(raw, decimals);
      return Number.isFinite(n) ? n : null;
    }
  }
}

/**
 * Turn change-only points into one value per bucket, holding the last value
 * until the next change. A null value (removed or unknown) breaks the line.
 */
export function carryForward(
  points: { t: string; value: number | null }[],
  interval: '1h' | '1d',
  fromIso: string,
  endIso: string,
): StepPoint[] {
  const bs = BUCKET_SECONDS[interval];
  const bucket = (iso: string) => {
    const ms = Date.parse(iso);
    return Number.isFinite(ms) ? Math.floor(ms / 1000 / bs) * bs : null;
  };
  const byBucket = new Map<number, number | null>();
  for (const p of [...points].sort((a, b) => a.t.localeCompare(b.t))) {
    const b = bucket(p.t);
    if (b != null) byBucket.set(b, p.value);
  }
  const keys = [...byBucket.keys()].sort((a, b) => a - b);
  const end = bucket(endIso);
  const start = bucket(fromIso);
  if (keys.length === 0 || end == null || start == null) return [];
  if ((end - keys[0]) / bs > MAX_BUCKETS) return [];
  const out: StepPoint[] = [];
  let cur: number | null = null;
  for (let t = keys[0]; t <= end; t += bs) {
    if (byBucket.has(t)) cur = byBucket.get(t) ?? null;
    if (t >= start) out.push({ time: t, value: cur });
  }
  return out;
}

export interface ConfigChange {
  label: string;
  from: string;
  to: string;
  at: string;
}

/** Collateral/liability factor and supply cap changes between consecutive points. */
export function configChanges(
  points: HistoryPoint[],
  configDecimals: number | null | undefined,
  tokenDecimals: number | null | undefined,
): ConfigChange[] {
  const out: ConfigChange[] = [];
  const sorted = [...points].sort((a, b) => a.t.localeCompare(b.t));
  const fields: {
    label: string;
    get: (p: HistoryPoint) => string | null;
    show: (raw: string) => string;
  }[] = [
    {
      label: 'Collateral factor',
      get: (p) => p.collateral_factor,
      show: (r) => fixedPct(r, configDecimals),
    },
    {
      label: 'Liability factor',
      get: (p) => p.liability_factor,
      show: (r) => fixedPct(r, configDecimals),
    },
    {
      label: 'Supply cap',
      get: (p) => p.supply_cap,
      show: (r) => supplyCapText(r, tokenDecimals),
    },
  ];
  for (let i = 1; i < sorted.length; i++) {
    for (const f of fields) {
      const a = f.get(sorted[i - 1]);
      const b = f.get(sorted[i]);
      if (a == null || b == null || a === b) continue;
      out.push({
        label: f.label,
        from: f.show(a),
        to: f.show(b),
        at: sorted[i].t,
      });
    }
  }
  return out;
}

export type AuctionStatus =
  | { kind: 'open'; label: string }
  | { kind: 'partial'; label: string; percent: number }
  | { kind: 'filled'; label: string; percent: number }
  | { kind: 'deleted'; label: string; percent: number };

/** Open, partly filled, fully filled, or deleted (removed unfilled or part-filled). */
export function auctionStatus(a: {
  deleted: boolean;
  fills?: { fill_percent: string }[] | null;
}): AuctionStatus {
  // Each fill_percent is a share of what was still open, so shares compound;
  // only a single 100 closes the auction (as the API's lifecycle walk does).
  let remaining = 1;
  let closed = false;
  for (const f of a.fills ?? []) {
    const n = Number(f.fill_percent);
    if (!Number.isFinite(n)) continue;
    if (n >= 100) closed = true;
    remaining *= 1 - Math.min(100, Math.max(0, n)) / 100;
  }
  const percent = closed ? 100 : Math.round((1 - remaining) * 10000) / 100;
  if (closed) return { kind: 'filled', label: 'Filled', percent };
  if (a.deleted) return { kind: 'deleted', label: 'Deleted', percent };
  if (percent > 0)
    return { kind: 'partial', label: `Filled ${percent}%`, percent };
  return { kind: 'open', label: 'Open' };
}

/** An amount map entry in whole tokens when its scale is known, else exact base units. */
export function amountText(
  raw: string,
  decimals: number | null | undefined,
): string {
  if (decimals == null) return `${raw} base units`;
  return formatBaseUnits(raw, decimals, 4);
}

/** The API's 503 "history unavailable" arrives as an Error whose text starts with the status. */
export function isHistoryBuilding(err: unknown): boolean {
  return err instanceof Error && err.message.startsWith('503');
}

export function tierLabel(tier: string | null | undefined): string | null {
  switch (tier) {
    case 'FirstLoss':
      return 'First-loss tier';
    case 'SecondLoss':
      return 'Second-loss tier';
    case 'ThirdLoss':
      return 'Third-loss tier';
    default:
      return null;
  }
}

export const BACKSTOP_ASSET_LABELS: Record<string, string> = {
  BlntXlm: 'BLNT / Stellar Lumens pool token',
  BlntUsdc: 'BLNT / USDC pool token',
  Usdc: 'USDC',
  Xlm: 'Stellar Lumens',
};
