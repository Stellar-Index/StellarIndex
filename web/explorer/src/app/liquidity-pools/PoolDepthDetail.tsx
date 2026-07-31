'use client';

import { DonutChart } from '@/components/charts/DonutChart';
import { HBarList } from '@/components/charts/Bars';
import { formatCompact } from '@/lib/format';
import { scaledUnits } from '../explorer-shared';

// Wire shapes for one /v1/liquidity-pools row (shared with
// NativePoolsPanel, which owns the fetch).
export interface ReserveSide {
  asset: string;
  decimals: number;
  reserve: string;
}

export interface DepthSide {
  max_input: string;
  output: string;
}

export interface DepthLevel {
  slippage_pct: string;
  asset_a_in: DepthSide;
  asset_b_in: DepthSide;
}

export interface PoolDepthRow {
  pool: string;
  fee_bps: number;
  as_of_ledger: number;
  reserve_a: ReserveSide;
  reserve_b: ReserveSide;
  mid_price_b_in_a: string | null;
  depth: DepthLevel[];
}

/**
 * Scale an exact base-unit integer string by 10^decimals for DISPLAY.
 * The API keeps reserves as exact decimal strings (ADR-0003); floats
 * appear only here, at the presentation edge.
 */
export function displayUnits(baseUnits: string, decimals: number): string {
  if (!/^\d+$/.test(baseUnits)) return '—';
  const padded = baseUnits.padStart(decimals + 1, '0');
  const whole = padded.slice(0, padded.length - decimals) || '0';
  const frac = decimals > 0 ? padded.slice(padded.length - decimals) : '';
  const n = Number(`${whole}.${frac || '0'}`);
  if (!Number.isFinite(n)) return `${whole}`;
  if (n >= 1000) {
    return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 2 }).format(n);
  }
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(n);
}

/** Short label for a canonical asset_id ("native" → XLM; "CODE-ISSUER" → CODE). */
export function assetLabel(id: string): string {
  if (id === 'native') return 'XLM';
  const dash = id.indexOf('-');
  return dash > 0 ? id.slice(0, dash) : id;
}

/**
 * PoolDepthDetail — the expanded-row visual for one native liquidity
 * pool: per-direction slippage/depth bars (largest fill within each
 * slippage tier) plus a two-segment reserve-composition donut valued at
 * the pool's own current mid price. Replaces the former nested text
 * tables; the exact figures stay on every bar label (ADR-0003: geometry
 * is float, displayed amounts come from the exact strings).
 */
export function PoolDepthDetail({ row }: { row: PoolDepthRow }) {
  const a = assetLabel(row.reserve_a.asset);
  const b = assetLabel(row.reserve_b.asset);

  if (row.depth.length === 0) {
    return (
      <p className="text-xs text-ink-muted">
        One side of this pool is empty — no meaningful depth.
      </p>
    );
  }

  // Reserve composition, both sides valued in {a}-equivalents at the
  // pool's current mid price. A constant-product pool holds ≈50/50 by
  // construction — a skewed donut is the anomaly signal (one-sided or
  // draining pool), not the norm.
  const aVal = scaledUnits(row.reserve_a.reserve, row.reserve_a.decimals);
  const midBinA = row.mid_price_b_in_a ? Number(row.mid_price_b_in_a) : NaN;
  const bValInA =
    scaledUnits(row.reserve_b.reserve, row.reserve_b.decimals) * midBinA;
  const donutOk =
    Number.isFinite(aVal) && aVal > 0 && Number.isFinite(bValInA) && bValInA > 0;

  const directions = [
    {
      key: 'a-in',
      heading: `Sell ${a} → get ${b}`,
      items: row.depth.map((lvl) => ({
        label: `≤ ${lvl.slippage_pct}%`,
        value: scaledUnits(lvl.asset_a_in.max_input, row.reserve_a.decimals),
        display: `${displayUnits(lvl.asset_a_in.max_input, row.reserve_a.decimals)} ${a}`,
        annotation: `→ ${displayUnits(lvl.asset_a_in.output, row.reserve_b.decimals)} ${b}`,
      })),
    },
    {
      key: 'b-in',
      heading: `Sell ${b} → get ${a}`,
      items: row.depth.map((lvl) => ({
        label: `≤ ${lvl.slippage_pct}%`,
        value: scaledUnits(lvl.asset_b_in.max_input, row.reserve_b.decimals),
        display: `${displayUnits(lvl.asset_b_in.max_input, row.reserve_b.decimals)} ${b}`,
        annotation: `→ ${displayUnits(lvl.asset_b_in.output, row.reserve_a.decimals)} ${a}`,
      })),
    },
  ];

  return (
    <div className="space-y-4">
      <div className="grid gap-x-8 gap-y-4 lg:grid-cols-2">
        {/* Each direction is its own chart with its own unit ({a} vs {b}
            input amounts) — never one shared axis across different
            assets. */}
        {directions.map((d) => (
          <div key={d.key} className="space-y-1.5">
            <h4 className="text-[11px] font-medium uppercase tracking-wider text-ink-muted">
              {d.heading}
            </h4>
            <HBarList
              ariaLabel={`${d.heading}: largest fill within each slippage tier`}
              items={d.items}
            />
          </div>
        ))}
      </div>

      {donutOk && (
        <div className="space-y-1.5">
          <h4 className="text-[11px] font-medium uppercase tracking-wider text-ink-muted">
            Reserve composition — valued at the pool&apos;s mid price
          </h4>
          <DonutChart
            size={120}
            thickness={16}
            data={[
              { label: a, value: aVal },
              { label: `${b} (in ${a}-equivalents)`, value: bValInA },
            ]}
            formatValue={(n) => `≈${formatCompact(n)} ${a}`}
          />
          <p className="text-[11px] leading-relaxed text-ink-muted">
            A constant-product pool holds ≈50/50 by value at its own mid
            price — a skewed split signals a one-sided or draining pool.
          </p>
        </div>
      )}

      <p className="text-[11px] leading-relaxed text-ink-muted">
        Depth bars show the largest trade whose average execution price
        stays within the tier of the mid price, under the constant-product
        model ({row.fee_bps} bps fee on input), from reserves as of ledger{' '}
        {row.as_of_ledger.toLocaleString('en-US')} — a model estimate, not
        an order book. Pool id {row.pool}.
      </p>
    </div>
  );
}
