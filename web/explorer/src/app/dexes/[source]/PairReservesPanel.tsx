'use client';

import { Fragment, useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import { useLedgerFollow } from '@/lib/live/hooks';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import {
  Button,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
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
    return new Intl.NumberFormat('en-US', {
      notation: 'compact',
      maximumFractionDigits: 2,
    }).format(n);
  }
  return new Intl.NumberFormat('en-US', { maximumSignificantDigits: 6 }).format(
    n,
  );
}

// The XLM Stellar Asset Contract self-reports its symbol as "native".
function tokenLabel(t: ReserveToken): string {
  if (t.symbol === 'native') return 'XLM';
  return t.symbol || `${t.contract.slice(0, 4)}…${t.contract.slice(-4)}`;
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

  // Live (RT-2): refresh pool reserves + depth on each ledger close.
  useLedgerFollow(['/v1/pools/reserves']);
  const q = useQuery<PoolReservesRow[]>({
    queryKey: ['/v1/pools/reserves'],
    queryFn: async () => {
      const env = await apiGet<{ data: PoolReservesRow[] }>(
        '/v1/pools/reserves',
      );
      return env.data ?? [];
    },
    staleTime: 30_000,
  });

  const rows = (q.data ?? [])
    .slice()
    .sort((a, b) => tokenLabel(a.token0).localeCompare(tokenLabel(b.token0)));

  return (
    <Panel
      headingLevel={2}
      title="Pool reserves & depth"
      hint="Current reserves · depth is a constant-product estimate, not an order book"
      source={asExample('/v1/pools/reserves')}
      bodyClassName={rows.length > 0 ? '-mx-4 -mb-4' : undefined}
    >
      {q.isLoading && <Skeleton className="h-24" />}
      {q.isError && (
        <p className="text-ink-muted text-sm">
          Reserves unavailable right now.
        </p>
      )}
      {!q.isLoading && !q.isError && rows.length === 0 && (
        <p className="text-ink-muted text-sm">No captured pool state.</p>
      )}
      {rows.length > 0 && (
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>Pool</Th>
                <Th align="right">Token 0 reserve</Th>
                <Th align="right">Token 1 reserve</Th>
                <Th align="right">Mid price</Th>
                <Th align="right">As of ledger</Th>
                <Th aria-hidden />
              </tr>
            </THead>
            <TBody>
              {rows.map((row) => {
                const open = expanded === row.pool;
                const t0 = tokenLabel(row.token0);
                const t1 = tokenLabel(row.token1);
                return (
                  <Fragment key={row.pool}>
                    <TR
                      className="cursor-pointer"
                      onClick={() => setExpanded(open ? null : row.pool)}
                    >
                      <Td>
                        <span className="text-ink font-medium">
                          {t0} / {t1}
                        </span>{' '}
                        <Link
                          href={`/contracts/${row.pool}`}
                          onClick={(e) => e.stopPropagation()}
                          className="text-ink-muted hover:text-brand-600 font-mono text-xs"
                        >
                          {row.pool.slice(0, 4)}…{row.pool.slice(-4)}
                        </Link>
                      </Td>
                      <Td align="right" className="font-mono">
                        {displayUnits(row.token0.reserve, row.token0.decimals)}{' '}
                        {t0}
                      </Td>
                      <Td align="right" className="font-mono">
                        {displayUnits(row.token1.reserve, row.token1.decimals)}{' '}
                        {t1}
                      </Td>
                      <Td align="right" className="font-mono">
                        {row.mid_price_0_in_1
                          ? `${midPriceLabel(row.mid_price_0_in_1)} ${t1}/${t0}`
                          : '—'}
                      </Td>
                      <Td align="right" className="text-ink-muted font-mono">
                        {row.as_of_ledger.toLocaleString('en-US')}
                      </Td>
                      {/* The keyboard/AT path to the depth detail. The
                          row's onClick stays a mouse convenience; this
                          native <button> puts the expander in the tab
                          order and states its expanded/collapsed status
                          (WCAG 2.1.1 / 4.1.2). */}
                      <Td align="right" className="py-1.5">
                        <Button
                          variant="ghost"
                          size="sm"
                          aria-expanded={open}
                          aria-controls={`pair-depth-${row.pool}`}
                          onClick={(e) => {
                            e.stopPropagation();
                            setExpanded(open ? null : row.pool);
                          }}
                        >
                          {open ? 'Hide depth' : 'Depth'}
                        </Button>
                      </Td>
                    </TR>
                    {open && (
                      <tr className="bg-surface-subtle/50">
                        <td
                          colSpan={6}
                          id={`pair-depth-${row.pool}`}
                          className="px-4 py-3"
                        >
                          {row.depth.length === 0 ? (
                            <p className="text-ink-muted text-xs">
                              One side of this pool is empty — no meaningful
                              depth.
                            </p>
                          ) : (
                            <div className="space-y-2">
                              <Table className="max-w-2xl text-xs">
                                <THead>
                                  <tr>
                                    <Th className="h-8 px-3">
                                      Within slippage
                                    </Th>
                                    <Th align="right" className="h-8 px-3">
                                      Sell {t0} → get {t1}
                                    </Th>
                                    <Th align="right" className="h-8 px-3">
                                      Sell {t1} → get {t0}
                                    </Th>
                                  </tr>
                                </THead>
                                <TBody>
                                  {row.depth.map((lvl) => (
                                    <TR key={lvl.slippage_pct}>
                                      <Td className="px-3 py-1.5">
                                        {lvl.slippage_pct}%
                                      </Td>
                                      <Td
                                        align="right"
                                        className="px-3 py-1.5 font-mono"
                                      >
                                        {displayUnits(
                                          lvl.token0_in.max_input,
                                          row.token0.decimals,
                                        )}{' '}
                                        {t0} →{' '}
                                        {displayUnits(
                                          lvl.token0_in.output,
                                          row.token1.decimals,
                                        )}{' '}
                                        {t1}
                                      </Td>
                                      <Td
                                        align="right"
                                        className="px-3 py-1.5 font-mono"
                                      >
                                        {displayUnits(
                                          lvl.token1_in.max_input,
                                          row.token1.decimals,
                                        )}{' '}
                                        {t1} →{' '}
                                        {displayUnits(
                                          lvl.token1_in.output,
                                          row.token0.decimals,
                                        )}{' '}
                                        {t0}
                                      </Td>
                                    </TR>
                                  ))}
                                </TBody>
                              </Table>
                              <p className="text-ink-muted text-xs leading-relaxed">
                                Largest trade whose average execution price
                                stays within the tier of the mid price, under
                                the constant-product model ({row.fee_bps} bps
                                fee on input), from reserves as of ledger{' '}
                                {row.as_of_ledger.toLocaleString('en-US')}.
                                Token symbols are self-declared by the token
                                contracts.
                              </p>
                            </div>
                          )}
                        </td>
                      </tr>
                    )}
                  </Fragment>
                );
              })}
            </TBody>
          </Table>
        </div>
      )}
    </Panel>
  );
}
