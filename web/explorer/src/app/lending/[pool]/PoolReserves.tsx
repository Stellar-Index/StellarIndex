'use client';

import { useQuery } from '@tanstack/react-query';
import { useLedgerFollow } from '@/lib/live/hooks';

import { Panel } from '@/components/reveal';
import { Button, TBody, TR, Table, Td, Th, THead } from '@/components/ui';
import { AssetLink } from '@/components/AssetLink';
import { DonutChart } from '@/components/charts/DonutChart';
import { HBarList, PairedBars } from '@/components/charts/Bars';
import { apiGet, asExample } from '@/api/client';
import {
  decimalOrNull,
  compareDecimalStrings,
  formatCompactUnits,
  formatReadable,
  formatUsdWhole,
  sumDecimalStrings,
} from '@/lib/format';
import { scaledUnits } from '../../explorer-shared';
import { shortAssetText } from '@/lib/asset-label';

interface ReserveRow {
  asset: string;
  decimals: number;
  supplied: string;
  borrowed: string;
  supplied_usd: string | null;
  borrowed_usd: string | null;
  utilization_pct: number;
  borrow_apr: number | null;
  supply_apr: number | null;
}

interface ReservesResp {
  pool: string;
  tvl_usd: string | null;
  lower_bound?: boolean;
  reserves: ReserveRow[];
}

const usdFmt = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  maximumFractionDigits: 0,
});

function tokenAmount(base: string, decimals: number): string {
  // Reserve amounts are exact i128 base-unit strings (ADR-0003); scale
  // via the string-split path, never Number() on the raw integer, which
  // loses precision above 2^53.
  const n = scaledUnits(base, decimals);
  if (!Number.isFinite(n)) return base;
  return new Intl.NumberFormat('en-US', {
    notation: 'compact',
    maximumFractionDigits: 2,
  }).format(n);
}

function pct(f: number | null): string {
  return f == null ? '—' : `${(f * 100).toFixed(2)}%`;
}

export function PoolReserves({ pool }: { pool: string }) {
  // Live (RT-2): refresh lending-pool reserves on each ledger close.
  useLedgerFollow(['/v1/lending/pools/{pool}/reserves']);
  const q = useQuery<ReservesResp>({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool],
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: ReservesResp }>(
        `/v1/lending/pools/${encodeURIComponent(pool)}/reserves`,
        {},
      );
      return env.data;
    },
    staleTime: 60_000,
  });

  const reserves = q.data?.reserves ?? [];
  const priced = reserves
    .filter(
      (rv) =>
        rv.supplied_usd != null &&
        compareDecimalStrings(rv.supplied_usd, '0') === 1,
    )
    .sort(
      (a, b) =>
        compareDecimalStrings(b.supplied_usd ?? '0', a.supplied_usd ?? '0') ??
        0,
    );
  // The served tvl_usd is this same Σ supplied_usd; the exact client sum
  // only stands in when the pool response omits it.
  const totalUsd =
    q.data?.tvl_usd ?? sumDecimalStrings(priced.map((rv) => rv.supplied_usd));
  // Either signal makes the total partial: the served flag, or an unpriced
  // reserve the client-side fallback sum could not include.
  const unpriced = reserves.filter((rv) => rv.supplied_usd == null);
  const lowerBound = q.data?.lower_bound === true || unpriced.length > 0;
  const excluded =
    unpriced.length > 0
      ? unpriced.map((rv) => shortAssetText(rv.asset)).join(', ')
      : 'reserves';

  return (
    <Panel
      headingLevel={2}
      title="Reserve composition"
      hint="Real current-state TVL / utilisation / supply+borrow APR, decoded from the pool contract's Soroban storage."
      source={asExample(`/v1/lending/pools/${pool}/reserves`, {})}
      bodyClassName="space-y-3"
    >
      {q.data?.tvl_usd && (
        <div className="text-ink-body text-sm">
          Pool TVL:{' '}
          <span className="text-ink font-mono" title={q.data.tvl_usd}>
            {lowerBound && (
              <span className="text-ink-muted" aria-hidden>
                ≥{' '}
              </span>
            )}
            {formatReadable(q.data.tvl_usd, true) ?? '—'}
          </span>{' '}
          <span className="text-ink-muted">
            {lowerBound ? (
              <>
                (<strong>at least</strong> this — Σ supplied across priced
                reserves; excludes unpriced {excluded})
              </>
            ) : (
              '(Σ supplied across priced reserves)'
            )}
          </span>
        </div>
      )}
      {priced.length > 0 &&
        totalUsd != null &&
        compareDecimalStrings(totalUsd, '0') === 1 && (
          <DonutChart
            data={priced.map((rv) => ({
              id: rv.asset,
              label: shortAssetText(rv.asset),
              value: Number(rv.supplied_usd),
              decimal: rv.supplied_usd,
            }))}
            centerLabel={`${lowerBound ? '≥ ' : ''}$${formatCompactUnits(totalUsd)}`}
            centerSub="TVL"
            formatValue={(n) => usdFmt.format(n)}
          />
        )}
      {priced.length > 0 && (
        <div className="space-y-6 py-2">
          <div className="space-y-1.5">
            <h3 className="text-ink text-sm font-medium">
              Supplied vs borrowed — USD, priced reserves
            </h3>
            <PairedBars
              ariaLabel={`Supplied vs borrowed per priced reserve: ${priced
                .map(
                  (rv) =>
                    `${shortAssetText(rv.asset)} $${formatCompactUnits(rv.supplied_usd)} supplied, ${rv.borrowed_usd != null ? `$${formatCompactUnits(rv.borrowed_usd)}` : 'unpriced'} borrowed`,
                )
                .join('; ')}`}
              aLabel="Supplied"
              bLabel="Borrowed"
              rows={priced.map((rv) => ({
                id: rv.asset,
                label: shortAssetText(rv.asset),
                a: Number(rv.supplied_usd),
                b: decimalOrNull(rv.borrowed_usd),
                aDisplay: `$${formatCompactUnits(rv.supplied_usd)}`,
                bDisplay:
                  rv.borrowed_usd != null
                    ? `$${formatCompactUnits(rv.borrowed_usd)}`
                    : '—',
                title: rv.asset,
              }))}
            />
          </div>

          <div className="space-y-1.5">
            <h3 className="text-ink text-sm font-medium">
              Utilization — borrowed / supplied, fixed 0–100% scale
            </h3>
            <HBarList
              ariaLabel={`Utilization per reserve: ${reserves
                .map(
                  (rv) =>
                    `${shortAssetText(rv.asset)} ${rv.utilization_pct.toFixed(1)}%`,
                )
                .join(', ')}`}
              max={100}
              items={reserves.map((rv) => ({
                id: rv.asset,
                label: shortAssetText(rv.asset),
                value: Math.max(0, Math.min(100, rv.utilization_pct)),
                display: `${rv.utilization_pct.toFixed(1)}%`,
                color:
                  rv.utilization_pct >= 90
                    ? 'var(--color-warn-500)'
                    : 'var(--color-brand-500)',
                annotation: rv.utilization_pct >= 90 ? 'near cap' : undefined,
                title: rv.asset,
              }))}
            />
          </div>

          {reserves.some(
            (rv) => rv.supply_apr != null || rv.borrow_apr != null,
          ) && (
            <div className="space-y-1.5">
              <h3 className="text-ink text-sm font-medium">
                Interest rates — supply vs borrow APR
              </h3>
              <PairedBars
                ariaLabel={`APR per reserve: ${reserves
                  .filter(
                    (rv) => rv.supply_apr != null || rv.borrow_apr != null,
                  )
                  .map(
                    (rv) =>
                      `${shortAssetText(rv.asset)} supply ${pct(rv.supply_apr)}, borrow ${pct(rv.borrow_apr)}`,
                  )
                  .join('; ')}`}
                aLabel="Supply APR"
                bLabel="Borrow APR"
                formatValue={(n) => `${(n * 100).toFixed(2)}%`}
                rows={reserves
                  .filter(
                    (rv) => rv.supply_apr != null || rv.borrow_apr != null,
                  )
                  .map((rv) => ({
                    id: rv.asset,
                    label: shortAssetText(rv.asset),
                    a: rv.supply_apr ?? null,
                    b: rv.borrow_apr ?? null,
                    aDisplay: pct(rv.supply_apr),
                    bDisplay: pct(rv.borrow_apr),
                    title: rv.asset,
                  }))}
              />
            </div>
          )}
        </div>
      )}

      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading reserve state…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Reserve state is unavailable right now (the contract-storage capture
          is still filling, or this isn&apos;t a reserve-bearing pool).
          <Button
            variant="secondary"
            size="sm"
            className="ml-3"
            onClick={() => void q.refetch()}
          >
            Retry
          </Button>
        </p>
      )}
      {q.data && reserves.length === 0 && !q.isLoading && (
        <p className="text-ink-muted text-sm">
          No reserve state captured for this pool yet — the lake&apos;s
          contract-storage window hasn&apos;t recorded its reserves.
        </p>
      )}
      {reserves.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Asset</Th>
                <Th align="right">Supplied</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Borrowed
                </Th>
                <Th align="right">Util</Th>
                <Th align="right" className="hidden sm:table-cell">
                  Supply APR
                </Th>
                <Th align="right" className="hidden sm:table-cell">
                  Borrow APR
                </Th>
              </tr>
            </THead>
            <TBody>
              {reserves.map((rv) => (
                <TR key={rv.asset}>
                  <Td className="whitespace-nowrap" title={rv.asset}>
                    <AssetLink canonical={rv.asset} />
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {rv.supplied_usd ? (
                      <span
                        title={`${tokenAmount(rv.supplied, rv.decimals)} tokens`}
                      >
                        {formatUsdWhole(rv.supplied_usd)}
                      </span>
                    ) : (
                      tokenAmount(rv.supplied, rv.decimals)
                    )}
                  </Td>
                  <Td
                    align="right"
                    className="hidden font-mono tabular-nums sm:table-cell"
                  >
                    {rv.borrowed_usd
                      ? formatUsdWhole(rv.borrowed_usd)
                      : tokenAmount(rv.borrowed, rv.decimals)}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {rv.utilization_pct.toFixed(1)}%
                  </Td>
                  <Td
                    align="right"
                    className="hidden font-mono tabular-nums sm:table-cell"
                  >
                    {pct(rv.supply_apr)}
                  </Td>
                  <Td
                    align="right"
                    className="hidden font-mono tabular-nums sm:table-cell"
                  >
                    {pct(rv.borrow_apr)}
                  </Td>
                </TR>
              ))}
            </TBody>
          </Table>
        </div>
      )}
      <p className="text-ink-muted text-xs">
        Exact current state from on-chain b_rate/d_rate. APR shows{' '}
        <span className="font-mono">—</span> when the rate config is outside the
        captured storage window.
      </p>
    </Panel>
  );
}
