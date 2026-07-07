'use client';

import { Fragment, useState } from 'react';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';

interface ReserveSide {
  asset: string;
  decimals: number;
  reserve: string;
}

interface DepthSide {
  max_input: string;
  output: string;
}

interface DepthLevel {
  slippage_pct: string;
  asset_a_in: DepthSide;
  asset_b_in: DepthSide;
}

interface LiquidityPoolRow {
  pool: string;
  pool_hex: string;
  model: string;
  fee_bps: number;
  as_of_ledger: number;
  trustlines: number;
  total_shares: string;
  reserve_a: ReserveSide;
  reserve_b: ReserveSide;
  mid_price_a_in_b: string | null;
  mid_price_b_in_a: string | null;
  depth: DepthLevel[];
}

/**
 * Scale an exact base-unit integer string by 10^decimals for DISPLAY.
 * The API keeps reserves as exact decimal strings (ADR-0003); floats
 * appear only here, at the presentation edge.
 */
function displayUnits(baseUnits: string, decimals: number): string {
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
function assetLabel(id: string): string {
  if (id === 'native') return 'XLM';
  const dash = id.indexOf('-');
  return dash > 0 ? id.slice(0, dash) : id;
}

function midPriceLabel(mid: string | null): string {
  if (!mid) return '—';
  const n = Number(mid);
  if (!Number.isFinite(n) || n === 0) return mid;
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(n);
}

/**
 * NativePoolsPanel — CURRENT two-sided reserves + constant-product
 * depth for Stellar's protocol-native (CAP-38) liquidity pools, from
 * /v1/liquidity-pools (an ADR-0039 read of the `liquidity_pool`
 * LedgerEntry in the certified lake).
 *
 * Default view: the top native pools by number of liquidity providers
 * (pool-share trustlines). The lookup box resolves a single pool by id
 * (L-strkey or 32-byte hex). Depth is model-derived (x·y=k with the
 * pool's on-chain fee on input), an estimate from current reserves —
 * not an order book. Rows expand to the per-tier depth table.
 */
export function NativePoolsPanel() {
  const [expanded, setExpanded] = useState<string | null>(null);
  const [input, setInput] = useState('');
  const [lookup, setLookup] = useState('');

  const q = useQuery<LiquidityPoolRow[]>({
    queryKey: ['/v1/liquidity-pools', lookup],
    queryFn: async () => {
      const path = lookup ? `/v1/liquidity-pools?pool=${encodeURIComponent(lookup)}` : '/v1/liquidity-pools';
      const env = await apiGet<{ data: LiquidityPoolRow[] }>(path);
      return env.data ?? [];
    },
    staleTime: 30_000,
    retry: false,
  });

  const rows = q.data ?? [];

  return (
    <Panel
      title="Native pool reserves & depth (current)"
      hint="Live two-sided reserves read from each native pool's ledger entry in the certified lake. Depth is a constant-product model estimate from current reserves (fee on input) — not an order book. The listing ranks pools by number of liquidity providers."
      source={asExample('/v1/liquidity-pools')}
    >
      <form
        className="mb-4 flex flex-wrap gap-2"
        onSubmit={(e) => {
          e.preventDefault();
          setExpanded(null);
          setLookup(input.trim());
        }}
      >
        <input
          type="text"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="Look up a pool by id (L… or 64-char hex)"
          className="min-w-0 flex-1 rounded-md border border-line bg-surface px-3 py-1.5 font-mono text-xs"
          aria-label="Native liquidity-pool id"
        />
        <button
          type="submit"
          className="rounded-md border border-line px-3 py-1.5 text-sm font-medium hover:bg-surface-subtle"
        >
          Look up
        </button>
        {lookup && (
          <button
            type="button"
            className="rounded-md px-3 py-1.5 text-sm text-ink-muted hover:text-brand-600"
            onClick={() => {
              setInput('');
              setLookup('');
              setExpanded(null);
            }}
          >
            Clear
          </button>
        )}
      </form>

      {q.isLoading && <p className="text-sm text-ink-muted">Loading reserves…</p>}
      {q.isError && (
        <p className="text-sm text-ink-muted">
          {lookup
            ? 'No native pool found for that id.'
            : 'Reserves unavailable right now.'}
        </p>
      )}
      {!q.isLoading && !q.isError && rows.length === 0 && (
        <p className="text-sm text-ink-muted">No captured native pool state.</p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-b border-line text-left text-xs uppercase tracking-wider text-ink-muted">
                <th className="py-2 pr-3 font-medium">Pool</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve A</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve B</th>
                <th className="py-2 pr-3 text-right font-medium">Mid price</th>
                <th className="py-2 pr-3 text-right font-medium">LPs</th>
                <th className="py-2 pr-3 text-right font-medium">As of ledger</th>
                <th className="py-2 font-medium" aria-hidden />
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const open = expanded === row.pool;
                const a = assetLabel(row.reserve_a.asset);
                const b = assetLabel(row.reserve_b.asset);
                return (
                  <Fragment key={row.pool}>
                    <tr
                      className="cursor-pointer border-b border-line/60 hover:bg-surface-subtle"
                      onClick={() => setExpanded(open ? null : row.pool)}
                    >
                      <td className="py-2 pr-3">
                        <span className="font-medium">{a} / {b}</span>{' '}
                        <span
                          className="font-mono text-xs text-ink-muted"
                          title={row.pool}
                        >
                          {row.pool.slice(0, 4)}…{row.pool.slice(-4)}
                        </span>
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(row.reserve_a.reserve, row.reserve_a.decimals)} {a}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(row.reserve_b.reserve, row.reserve_b.decimals)} {b}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {row.mid_price_a_in_b
                          ? `${midPriceLabel(row.mid_price_a_in_b)} ${b}/${a}`
                          : '—'}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums text-ink-muted">
                        {row.trustlines.toLocaleString('en-US')}
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
                        <td colSpan={7} className="px-3 py-3">
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
                                    <th className="py-1 pr-3 text-right font-medium">Sell {a} → get {b}</th>
                                    <th className="py-1 text-right font-medium">Sell {b} → get {a}</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {row.depth.map((lvl) => (
                                    <tr key={lvl.slippage_pct} className="border-t border-line/40">
                                      <td className="py-1 pr-3">{lvl.slippage_pct}%</td>
                                      <td className="py-1 pr-3 text-right tabular-nums">
                                        {displayUnits(lvl.asset_a_in.max_input, row.reserve_a.decimals)} {a} →{' '}
                                        {displayUnits(lvl.asset_a_in.output, row.reserve_b.decimals)} {b}
                                      </td>
                                      <td className="py-1 text-right tabular-nums">
                                        {displayUnits(lvl.asset_b_in.max_input, row.reserve_b.decimals)} {b} →{' '}
                                        {displayUnits(lvl.asset_b_in.output, row.reserve_a.decimals)} {a}
                                      </td>
                                    </tr>
                                  ))}
                                </tbody>
                              </table>
                              <p className="text-[11px] leading-relaxed text-ink-muted">
                                Largest trade whose average execution price stays within the tier of the mid
                                price, under the constant-product model ({row.fee_bps} bps fee on input),
                                from reserves as of ledger {row.as_of_ledger.toLocaleString('en-US')}.
                                Pool id {row.pool}.
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
