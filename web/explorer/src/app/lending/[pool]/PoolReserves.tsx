'use client';

import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { AssetLink } from '@/components/AssetLink';
import { DonutChart } from '@/components/charts/DonutChart';
import { apiGet, asExample } from '@/api/client';
import { formatCompact } from '@/lib/format';

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
  reserves: ReserveRow[];
}

const usdFmt = new Intl.NumberFormat('en-US', { style: 'currency', currency: 'USD', maximumFractionDigits: 0 });

function shortAsset(asset: string): string {
  return `${asset.slice(0, 4)}…${asset.slice(-4)}`;
}

function tokenAmount(base: string, decimals: number): string {
  const n = Number(base) / 10 ** decimals;
  if (!Number.isFinite(n)) return base;
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 2 }).format(n);
}

function pct(f: number | null): string {
  return f == null ? '—' : `${(f * 100).toFixed(2)}%`;
}

export function PoolReserves({ pool }: { pool: string }) {
  const q = useQuery<ReservesResp>({
    queryKey: ['/v1/lending/pools/{pool}/reserves', pool],
    retry: false,
    queryFn: async () => {
      const env = await apiGet<{ data: ReservesResp }>(`/v1/lending/pools/${encodeURIComponent(pool)}/reserves`, {});
      return env.data;
    },
    staleTime: 60_000,
  });

  const reserves = q.data?.reserves ?? [];
  const priced = reserves
    .filter((rv) => rv.supplied_usd != null && Number(rv.supplied_usd) > 0)
    .sort((a, b) => Number(b.supplied_usd) - Number(a.supplied_usd));
  const totalUsd = priced.reduce((sum, rv) => sum + Number(rv.supplied_usd), 0);

  return (
    <Panel
      title="Reserve composition"
      hint="Real current-state TVL / utilisation / supply+borrow APY, decoded from the pool contract's Soroban storage (ADR-0039)."
      source={asExample(`/v1/lending/pools/${pool}/reserves`, {})}
      bodyClassName="space-y-3"
    >
      {q.data?.tvl_usd && (
        <div className="text-sm text-ink-body">
          Pool TVL: <span className="font-mono text-ink">{usdFmt.format(Number(q.data.tvl_usd))}</span>{' '}
          <span className="text-ink-muted">(Σ supplied across priced reserves)</span>
        </div>
      )}
      {priced.length > 0 && totalUsd > 0 && (
        <DonutChart
          data={priced.map((rv) => ({
            label: shortAsset(rv.asset),
            value: Number(rv.supplied_usd),
          }))}
          centerLabel={`$${formatCompact(totalUsd)}`}
          centerSub="TVL"
          formatValue={(n) => usdFmt.format(n)}
        />
      )}
      {q.isLoading && <p className="text-sm text-ink-muted">Loading reserve state…</p>}
      {q.isError && (
        <p className="text-sm text-ink-muted">
          Reserve state is unavailable right now (the contract-storage capture is still filling, or this isn&apos;t a
          reserve-bearing pool).
        </p>
      )}
      {q.data && reserves.length === 0 && !q.isLoading && (
        <p className="text-sm text-ink-muted">
          No reserve state captured for this pool yet — the lake&apos;s contract-storage window hasn&apos;t recorded its
          reserves.
        </p>
      )}
      {reserves.length > 0 && (
        <div className="overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-[11px] uppercase tracking-wider text-ink-muted">
                <th className="py-1.5 pr-4 font-normal">Asset</th>
                <th className="py-1.5 pr-4 text-right font-normal">Supplied</th>
                <th className="py-1.5 pr-4 text-right font-normal">Borrowed</th>
                <th className="py-1.5 pr-4 text-right font-normal">Util</th>
                <th className="py-1.5 pr-4 text-right font-normal">Supply APR</th>
                <th className="py-1.5 text-right font-normal">Borrow APR</th>
              </tr>
            </thead>
            <tbody>
              {reserves.map((rv) => (
                <tr key={rv.asset} className="border-b border-line/60 last:border-0 hover:bg-surface-muted">
                  <td className="py-1.5 pr-4 font-mono text-[11px]" title={rv.asset}>
                    <AssetLink canonical={rv.asset} />
                  </td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                    {rv.supplied_usd ? (
                      <span title={`${tokenAmount(rv.supplied, rv.decimals)} tokens`}>
                        {usdFmt.format(Number(rv.supplied_usd))}
                      </span>
                    ) : (
                      tokenAmount(rv.supplied, rv.decimals)
                    )}
                  </td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                    {rv.borrowed_usd ? usdFmt.format(Number(rv.borrowed_usd)) : tokenAmount(rv.borrowed, rv.decimals)}
                  </td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums">
                    <div>{rv.utilization_pct.toFixed(1)}%</div>
                    <div className="ml-auto mt-1 h-1 w-16 overflow-hidden rounded-full bg-surface-muted">
                      <div
                        className="h-full rounded-full bg-brand-500"
                        style={{ width: `${Math.max(0, Math.min(100, rv.utilization_pct))}%` }}
                      />
                    </div>
                  </td>
                  <td className="py-1.5 pr-4 text-right font-mono tabular-nums text-up-strong">{pct(rv.supply_apr)}</td>
                  <td className="py-1.5 text-right font-mono tabular-nums text-down-strong">{pct(rv.borrow_apr)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      <p className="text-[11px] text-ink-muted">
        Supplied / borrowed / utilisation are exact current-state from the reserve&apos;s on-chain b_rate/d_rate.
        APR (the pool&apos;s own interest-rate model) shows when the reserve&apos;s rate config is in the captured
        storage window, else <span className="font-mono">—</span>. USD values are shown for reserves we hold a price
        for. Distinct from the auction-stream window proxy on the pools list.
      </p>
    </Panel>
  );
}
