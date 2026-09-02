'use client';

import { Fragment, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useLedgerFollow } from '@/lib/live/hooks';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import {
  PoolDepthDetail,
  type PoolDepthRow,
  assetLabel,
  displayUnits,
} from './PoolDepthDetail';
import { Button, Mono } from '@/components/ui';

interface LiquidityPoolRow extends PoolDepthRow {
  pool_hex: string;
  model: string;
  trustlines: number;
  total_shares: string;
  mid_price_a_in_b: string | null;
}

function midPriceLabel(mid: string | null): string {
  if (!mid) return '—';
  const n = Number(mid);
  if (!Number.isFinite(n) || n === 0) return mid;
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(
    n,
  );
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

  // Live (RT-2): refresh pool reserves + depth on each ledger close.
  useLedgerFollow(['/v1/liquidity-pools']);
  const q = useQuery<LiquidityPoolRow[]>({
    queryKey: ['/v1/liquidity-pools', lookup],
    queryFn: async () => {
      const path = lookup
        ? `/v1/liquidity-pools?pool=${encodeURIComponent(lookup)}`
        : '/v1/liquidity-pools';
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
          className="border-line bg-surface min-w-0 flex-1 rounded-md border px-3 py-1.5 font-mono text-xs"
          aria-label="Native liquidity-pool id"
        />
        <Button type="submit" variant="secondary" size="sm">
          Look up
        </Button>
        {lookup && (
          <button
            type="button"
            className="text-ink-muted hover:text-brand-600 rounded-md px-3 py-1.5 text-sm"
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

      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading reserves…</p>
      )}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          {lookup
            ? 'No native pool found for that id.'
            : 'Reserves unavailable right now.'}
        </p>
      )}
      {!q.isLoading && !q.isError && rows.length === 0 && (
        <p className="text-ink-muted text-sm">No captured native pool state.</p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <table className="w-full text-sm">
            <thead>
              <tr className="border-line text-ink-muted border-b text-left text-xs tracking-wider uppercase">
                <th className="py-2 pr-3 font-medium">Pool</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve A</th>
                <th className="py-2 pr-3 text-right font-medium">Reserve B</th>
                <th className="py-2 pr-3 text-right font-medium">Mid price</th>
                <th className="py-2 pr-3 text-right font-medium">LPs</th>
                <th className="py-2 pr-3 text-right font-medium">
                  As of ledger
                </th>
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
                      className="border-line/60 hover:bg-surface-subtle cursor-pointer border-b"
                      onClick={() => setExpanded(open ? null : row.pool)}
                    >
                      <td className="py-2 pr-3">
                        <span className="font-medium">
                          {a} / {b}
                        </span>{' '}
                        {/* The full pool id was hover-only (`title=`),
                            unreachable by touch/keyboard. Mono keeps the
                            hover title AND adds the canonical copy
                            button, which is the reachable path. */}
                        <Mono
                          value={row.pool}
                          truncate={{ head: 4, tail: 4 }}
                          className="text-ink-muted text-xs"
                        />
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(
                          row.reserve_a.reserve,
                          row.reserve_a.decimals,
                        )}{' '}
                        {a}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {displayUnits(
                          row.reserve_b.reserve,
                          row.reserve_b.decimals,
                        )}{' '}
                        {b}
                      </td>
                      <td className="py-2 pr-3 text-right tabular-nums">
                        {row.mid_price_a_in_b
                          ? `${midPriceLabel(row.mid_price_a_in_b)} ${b}/${a}`
                          : '—'}
                      </td>
                      <td className="text-ink-muted py-2 pr-3 text-right tabular-nums">
                        {row.trustlines.toLocaleString('en-US')}
                      </td>
                      <td className="text-ink-muted py-2 pr-3 text-right tabular-nums">
                        {row.as_of_ledger.toLocaleString('en-US')}
                      </td>
                      {/* The keyboard/AT path to the depth detail. The
                          row's onClick stays a mouse convenience; this
                          native <button> puts the expander in the tab
                          order and states its expanded/collapsed status
                          (WCAG 2.1.1 / 4.1.2). */}
                      <td className="py-2 text-right text-xs">
                        <button
                          type="button"
                          aria-expanded={open}
                          aria-controls={`pool-depth-${row.pool}`}
                          onClick={(e) => {
                            e.stopPropagation();
                            setExpanded(open ? null : row.pool);
                          }}
                          className="text-ink-muted hover:text-brand-600 focus-visible:ring-brand-500/60 rounded-sm px-1 py-0.5 transition-colors focus-visible:ring-2 focus-visible:outline-hidden"
                        >
                          {open ? 'Hide depth ▴' : 'Depth ▾'}
                        </button>
                      </td>
                    </tr>
                    {open && (
                      <tr className="border-line/60 bg-surface-subtle/50 border-b">
                        <td
                          colSpan={7}
                          id={`pool-depth-${row.pool}`}
                          className="px-3 py-3"
                        >
                          <PoolDepthDetail row={row} />
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
