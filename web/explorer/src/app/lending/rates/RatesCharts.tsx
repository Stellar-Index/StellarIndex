'use client';

import { useState } from 'react';

import { Segmented } from '@/components/ui';
import { HBarList, type HBarItem } from '@/components/charts/Bars';
import { Scatter, type ScatterPoint } from '@/components/charts/Scatter';
import { shortAssetText } from '@/lib/asset-label';

import {
  PROTOCOL_COLOR,
  plotNumber,
  protocolColor,
  usdLabel,
} from '../chartColors';
import { LENDING_POOL_META, lendingProtocol } from '../poolMeta';
import type { LendingRate } from './LendingRatesTable';

const assetKey = (r: LendingRate) => r.asset_id ?? r.asset ?? '';

export type AprMetric = 'supply' | 'borrow';

const pctText = (f: number) => `${(f * 100).toFixed(2)}%`;
const poolName = (r: LendingRate) =>
  LENDING_POOL_META[r.pool ?? '']?.name ??
  `${lendingProtocol(r.protocol)?.name ?? 'Lending'} pool ${(r.pool ?? '').slice(0, 4)}…${(r.pool ?? '').slice(-4)}`;
const aprOf = (r: LendingRate, m: AprMetric) =>
  m === 'supply' ? r.supply_apr : r.borrow_apr;

/**
 * One dot per reserve: x utilisation, y the chosen APR, area by supplied USD.
 * A reserve without supplied USD is left off (never drawn at size zero) and
 * counted in `unpriced`; one without the APR or utilisation in `unrated`.
 */
export function scatterPoints(rows: LendingRate[], metric: AprMetric) {
  const points: ScatterPoint[] = [];
  let unpriced = 0;
  let unrated = 0;
  for (const r of rows) {
    const apr = aprOf(r, metric);
    const size = plotNumber(r.supplied_usd);
    if (apr == null || r.utilization_pct == null) {
      unrated++;
      continue;
    }
    if (size == null) {
      unpriced++;
      continue;
    }
    const usd = usdLabel(r.supplied_usd);
    points.push({
      id: `${r.pool}:${r.asset}`,
      x: r.utilization_pct,
      y: apr * 100,
      size,
      color: protocolColor(r.protocol),
      hollow: r.obtainable === false,
      lines: [
        `${shortAssetText(assetKey(r))} · ${poolName(r)}`,
        `Supply ${r.supply_apr == null ? '—' : pctText(r.supply_apr)} · Borrow ${r.borrow_apr == null ? '—' : pctText(r.borrow_apr)}`,
        `Utilisation ${r.utilization_pct.toFixed(1)}%`,
        `Supplied ${usd?.exact ?? '—'}`,
        ...(r.obtainable === false ? ['Not obtainable'] : []),
      ],
    });
  }
  return { points, unpriced, unrated };
}

/** Supply APR of one asset across pools, highest first. */
export function assetAprItems(rows: LendingRate[]): HBarItem[] {
  return rows
    .filter((r) => r.supply_apr != null)
    .sort((a, b) => (b.supply_apr ?? 0) - (a.supply_apr ?? 0))
    .map((r) => ({
      id: `${r.pool}:${r.asset}`,
      label: poolName(r),
      value: (r.supply_apr ?? 0) * 100,
      display: pctText(r.supply_apr ?? 0),
      annotation:
        r.obtainable === false
          ? 'not obtainable'
          : lendingProtocol(r.protocol)?.name,
      color: protocolColor(r.protocol),
      title: r.pool ?? undefined,
    }));
}

function ProtocolKey() {
  return (
    <ul className="text-ink-muted flex flex-wrap gap-x-4 gap-y-1 text-xs">
      {(['blend', 'blnt'] as const).map((p) => (
        <li key={p} className="flex items-center gap-1.5">
          <span
            aria-hidden
            className="inline-block h-2 w-2 rounded-full"
            style={{ backgroundColor: PROTOCOL_COLOR[p] }}
          />
          {lendingProtocol(p)?.name ?? p}
        </li>
      ))}
      <li className="flex items-center gap-1.5">
        <span
          aria-hidden
          className="border-ink-muted inline-block h-2 w-2 rounded-full border-2"
        />
        Not obtainable
      </li>
      <li>Dot area: supplied USD</li>
    </ul>
  );
}

/** The rates page's lead: yield against utilisation, and one asset across pools. */
export function RatesCharts({
  rows,
  asset,
}: {
  rows: LendingRate[];
  asset: string;
}) {
  const [metric, setMetric] = useState<AprMetric>('supply');
  const { points, unpriced, unrated } = scatterPoints(rows, metric);
  const bars = asset ? assetAprItems(rows) : [];
  const label = metric === 'supply' ? 'Supply APR' : 'Borrow APR';
  return (
    <div className="mx-4 mb-4 space-y-4">
      <div className="flex flex-wrap items-center gap-3">
        <Segmented
          ariaLabel="APR shown"
          value={metric}
          onChange={(v) => setMetric(v as AprMetric)}
          options={[
            { value: 'supply', label: 'Supply APR' },
            { value: 'borrow', label: 'Borrow APR' },
          ]}
        />
        <ProtocolKey />
      </div>
      {points.length === 0 ? (
        <p className="text-ink-muted text-sm">
          No priced reserve with a rate matches this filter.
        </p>
      ) : (
        <Scatter
          points={points}
          xLabel="Utilisation"
          yLabel={label}
          formatX={(n) => `${n.toFixed(0)}%`}
          formatY={(n) => `${n.toFixed(n < 10 ? 1 : 0)}%`}
          ariaLabel={`${label} against utilisation for ${points.length} reserves; the table below lists every value`}
        />
      )}
      {(unpriced > 0 || unrated > 0) && (
        <p role="status" className="text-ink-muted text-xs">
          {unpriced > 0 && `${unpriced} not priced, not shown. `}
          {unrated > 0 &&
            `${unrated} without a ${label.toLowerCase()} or utilisation, not shown.`}
        </p>
      )}
      {bars.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-ink text-sm font-medium">
            Supply APR for {shortAssetText(asset)} by pool
          </h3>
          <HBarList
            items={bars}
            formatValue={(n) => `${n.toFixed(2)}%`}
            ariaLabel={`Supply APR for ${shortAssetText(asset)}: ${bars.map((b) => `${b.label} ${b.display}`).join(', ')}`}
          />
        </div>
      )}
    </div>
  );
}
