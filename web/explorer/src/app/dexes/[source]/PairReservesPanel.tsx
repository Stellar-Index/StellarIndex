'use client';

import { Fragment, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';

interface ReserveToken {
  contract: string;
  symbol?: string;
  decimals: number;
  reserve: string;
}

interface DepthSide {
  max_input: string;
  output: string;
}

interface DepthLevel {
  slippage_pct: string;
  token0_in: DepthSide;
  token1_in: DepthSide;
}

interface PoolReservesRow {
  pool: string;
  source: string;
  model: string;
  fee_bps: number;
  as_of_ledger: number;
  token0: ReserveToken;
  token1: ReserveToken;
  mid_price_0_in_1: string | null;
  mid_price_1_in_0: string | null;
  depth: DepthLevel[];
}

/**
 * Scale an exact base-unit integer string by 10^decimals for DISPLAY.
 * The API keeps reserves as exact i128 decimal strings (ADR-0003);
 * floats appear only here, at the presentation edge.
 */
function displayUnits(baseUnits: string, decimals: number): string {
  if (!/^\d+$/.test(baseUnits)) return '—';
  const padded = baseUnits.padStart(decimals + 1, '0');
  const whole = padded.slice(0, padded.length - decimals) || '0';
  const frac = decimals > 0 ? padded.slice(padded.length - decimals) : '';
  const n = Number(`${whole}.${frac || '0'}`);
  if (!Number.isFinite(n)) return `${whole}`; // beyond float range: whole part only
  if (n >= 1000) {
    return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 2 }).format(n);
  }
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(n);
}

function tokenLabel(t: ReserveToken): string {
  return t.symbol || `${t.contract.slice(0, 4)}…${t.contract.slice(-4)}`;
}

function midPriceLabel(mid: string | null): string {
  if (!mid) return '—';
  const n = Number(mid);
  if (!Number.isFinite(n) || n === 0) return mid;
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(n);
}

/**
 * PairReservesPanel — CURRENT on-chain reserves + constant-product
 * depth for every registered Soroswap pair, from /v1/pools/reserves
 * (an ADR-0039 contract-storage read of the certified lake).
 *
 * Honest-coverage: the endpoint serves Soroswap only — the one venue
 * whose pool-storage layout is verified. Depth figures are
 * model-derived (x·y=k with the 30 bps fee on input), labelled as
 * such; they are estimates from current reserves, not an order book.
 * Rows expand to the per-tier depth table.
 */
export function PairReservesPanel() {
  const [expanded, setExpanded] = useState<string | null>(null);

  const q = useQuery<PoolReservesRow[]>({
    queryKey: ['/v1/pools/reserves'],
    queryFn: async () => {
      const env = await apiGet<{ data: PoolReservesRow[] }>('/v1/pools/reserves');
      return env.data ?? [];
    },
    staleTime: 30_000,
  });

  const rows = (q.data ?? [])
    .slice()
    .sort((a, b) => tokenLabel(a.token0).localeCompare(tokenLabel(b.token0)));

  return (
    <Panel
      title="Pool reserves & depth (current)"
      hint="Live contract-storage read from the certified lake. Depth is a constant-product model estimate from current reserves (0.3% fee on input) — not an order book. Served for Soroswap only: the one venue whose pool-storage layout is verified."
      source={asExample('/v1/pools/reserves')}
    >
      {q.isLoading && <p className="text-sm text-ink-muted">Loading reserves…</p>}
      {q.isError && (
        <p className="text-sm text-ink-muted">
          Reserves unavailable right now.
        </p>
      )}
      {!q.isLoading && !q.isError && rows.length === 0 && (
        <p className="text-sm text-ink-muted">No captured pool state.</p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-xs uppercase tracking-wider text-ink-muted">
                <th className="py-2 pr-3 font-medium">Pool</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve 0</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve 1</th>
                <th className="py-2 pr-3 text-right font-medium">Mid price</th>
                <th className="py-2 pr-3 text-right font-medium">As of ledger</th>
                <th className="py-2 font-medium" aria-hidden />
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const open = expanded === row.pool;
                const t0 = tokenLabel(row.token0);
                const t1 = tokenLabel(row.token1);
                return (
                  <Fragment key={row.pool}>
                    <tr
                      className="cursor-pointer border-b border-line/60 hover:bg-surface-subtle"
                      onClick={() => setExpanded(open ? null : row.pool)}
                    >
                      <td className="py-2 pr-3">
                        <span className="font-medium">{t0} / {t1}</span>{' '}
                        <Link
                          href={`/contracts/${row.pool}`}
                          onClick={(e) => e.stopPropagation()}
                          className="font-mono text-xs text-ink-muted hover:text-brand-600"
                        >
                          {row.pool.slice(0, 4)}…{row.pool.slice(-4)}
                        </Link>
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(row.token0.reserve, row.token0.decimals)} {t0}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(row.token1.reserve, row.token1.decimals)} {t1}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {row.mid_price_0_in_1
                          ? `${midPriceLabel(row.mid_price_0_in_1)} ${t1}/${t0}`
                          : '—'}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums text-ink-muted">
                        {row.as_of_ledger.toLocaleString('en-US')}
                      </td>
                      <td className="py-2 text-right text-xs text-ink-muted">
                        {open ? 'Hide depth ▴' : 'Depth ▾'}
                      </td>
                    </tr>
                    {open && (
                      <tr className="border-b border-line/60 bg-surface-subtle/50">
                        <td colSpan={6} className="px-3 py-3">
                          {row.depth.length === 0 ? (
                            <p className="text-xs text-ink-muted">
                              One side of this pool is empty — no meaningful depth.
                            </p>
                          ) : (
                            <div className="space-y-2">
                              <table className="w-full max-w-2xl text-xs">
                                <thead>
                                  <tr className="text-left text-ink-muted">
                                    <th className="py-1 pr-3 font-medium">Within slippage</th>
                                    <th className="py-1 pr-3 text-right font-medium">Sell {t0} → get {t1}</th>
                                    <th className="py-1 text-right font-medium">Sell {t1} → get {t0}</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {row.depth.map((lvl) => (
                                    <tr key={lvl.slippage_pct} className="border-t border-line/40">
                                      <td className="py-1 pr-3">{lvl.slippage_pct}%</td>
                                      <td className="py-1 pr-3 text-right tabular-nums">
                                        {displayUnits(lvl.token0_in.max_input, row.token0.decimals)} {t0} →{' '}
                                        {displayUnits(lvl.token0_in.output, row.token1.decimals)} {t1}
                                      </td>
                                      <td className="py-1 text-right tabular-nums">
                                        {displayUnits(lvl.token1_in.max_input, row.token1.decimals)} {t1} →{' '}
                                        {displayUnits(lvl.token1_in.output, row.token0.decimals)} {t0}
                                      </td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                              <p className="text-[11px] leading-relaxed text-ink-muted">
                                Largest trade whose average execution price stays within the tier of the mid
                                price, under the constant-product model ({row.fee_bps} bps fee on input),
                                from reserves as of ledger {row.as_of_ledger.toLocaleString('en-US')}.
                                Token symbols are self-declared by the token contracts.
                              </p>
                            </div>
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}
