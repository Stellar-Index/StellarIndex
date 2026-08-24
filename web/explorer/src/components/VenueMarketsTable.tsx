'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { useLedgerFollow } from '@/lib/live/hooks';
import { LastPriceCell } from '@/components/LastPriceCell';
import { Panel } from '@/components/reveal';
import { AssetLabel } from '@/components/AssetLabel';
import { type Envelope, apiGet, asExample } from '@/api/client';
import { formatCompact, formatRelative } from '@/lib/format';
// /v1/markets row from the generated OpenAPI contract, via the shared
// alias in src/api/hooks.ts (Market = MarketRow) — the contract-derived
// type wins (FEC audit A3-F8): if /v1/markets changes, this build breaks
// loudly instead of a private interface silently diverging.
import type { Market } from '@/api/hooks';
import { useCursorPager } from '@/lib/useCursorPager';
import { SortPill } from '@/components/SortPill';

type Order = 'volume_24h_usd_desc' | 'pair';

const PAGE_LIMIT = 100;

/**
 * VenueMarketsTable — every (base, quote) pair a venue observed in the
 * trailing 14d, from /v1/markets?source= with cursor pagination.
 *
 * FEC audit A3-F8: dexes/[source]/PoolsTable and exchanges/[name]/
 * PairsTable were a 300-line whole-component fork of each other (same
 * endpoint, columns, pager, states) whose only real differences were the
 * Panel title, the pools/pairs copy nouns, and the Market type source —
 * and this exact pair had already forked-and-drifted once before
 * (LastPriceCell). One shared component; the caller supplies title +
 * rowNoun (which carries the S-023 "SDEX markets" special case).
 */
export function VenueMarketsTable({
  source,
  title,
  rowNoun,
}: {
  source: string;
  title: string;
  /** "pools" | "pairs" — loading/empty/unavailable copy. */
  rowNoun: string;
}) {
  const [order, setOrder] = useState<Order>('volume_24h_usd_desc');
  const pager = useCursorPager();
  const { cursor } = pager;

  // Live (RT-2): follow ledger closes so this venue's pairs + prices tick.
  useLedgerFollow(['/v1/markets', source]);
  const q = useQuery<{ markets: Market[]; nextCursor?: string }>({
    queryKey: ['/v1/markets', source, order, cursor],
    queryFn: async () => {
      const env = await apiGet<Envelope<Market[]>>('/v1/markets', {
        source,
        order_by: order,
        limit: PAGE_LIMIT,
        ...(cursor ? { cursor } : {}),
      });
      return {
        markets: env.data ?? [],
        nextCursor: env.pagination?.next,
      };
    },
  });

  function nextPage() {
    pager.next(q.data?.nextCursor);
  }
  function prevPage() {
    pager.prev();
  }
  function changeOrder(next: Order) {
    setOrder(next);
    pager.reset();
  }

  // Absent (request failed) ≠ empty (venue was quiet): /v1/markets
  // answers 503 on its documented 8s trades-hypertable ceiling, and
  // `q.data` is undefined for both that and the loading state.
  // Flattening to `[]` would claim "this venue traded nothing for 14
  // days". Keep absence visible.
  const markets = q.data?.markets;
  const rows = markets ?? [];
  const hasNext = !!q.data?.nextCursor;
  const hasPrev = pager.hasPrev;

  return (
    <Panel
      title={title}
      hint="One row per (base, quote) pair observed in the last 14 days"
      source={asExample('/v1/markets', {
        source,
        order_by: order,
        limit: PAGE_LIMIT,
      })}
      bodyClassName="-mx-4"
    >
      <div className="px-4 pt-1 pb-3">
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <span className="text-ink-muted">Sort:</span>
          <SortPill
            active={order === 'volume_24h_usd_desc'}
            onClick={() => changeOrder('volume_24h_usd_desc')}
          >
            24h volume ↓
          </SortPill>
          <SortPill
            active={order === 'pair'}
            onClick={() => changeOrder('pair')}
          >
            Pair (A→Z)
          </SortPill>
          <span className="text-ink-muted ml-auto font-mono text-[11px]">
            {markets ? `${markets.length} on this page` : '— on this page'}
            {q.isFetching && ' · refreshing…'}
          </span>
        </div>
      </div>

      <div className="overflow-x-auto">
        <table className="divide-line min-w-full divide-y text-sm">
          <thead>
            <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
              <Th>#</Th>
              <Th>Base</Th>
              <Th>Quote</Th>
              <Th align="right">Last price</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
              <Th align="right">Last trade</Th>
            </tr>
          </thead>
          <tbody className="divide-line-subtle divide-y">
            {q.isLoading && !q.data && (
              <tr>
                <td
                  colSpan={7}
                  className="text-ink-muted px-4 py-8 text-center text-sm"
                >
                  Loading {rowNoun}…
                </td>
              </tr>
            )}
            {!q.isLoading && !markets && (
              <tr>
                <td
                  colSpan={7}
                  className="text-ink-muted px-4 py-8 text-center text-sm"
                >
                  {rowNoun === 'pools' ? 'Pool' : 'Pair'} list unavailable right
                  now — the markets query didn&apos;t return. Retry shortly.
                </td>
              </tr>
            )}
            {!q.isLoading && markets && markets.length === 0 && (
              <tr>
                <td
                  colSpan={7}
                  className="text-ink-muted px-4 py-8 text-center text-sm"
                >
                  No {rowNoun} found in the last 14 days.
                </td>
              </tr>
            )}
            {rows.map((m, i) => {
              const slug = `${m.base}~${m.quote}`;
              const offset = pager.depth * PAGE_LIMIT + i + 1;
              const vol = m.volume_24h_usd ? Number(m.volume_24h_usd) : null;
              return (
                <tr
                  key={`${m.base}|${m.quote}`}
                  className="hover:bg-surface-muted"
                >
                  <Td>
                    <span className="text-ink-faint font-mono text-[11px]">
                      {offset}
                    </span>
                  </Td>
                  <Td>
                    <Link
                      href={`/markets/${encodeURIComponent(slug)}`}
                      className="hover:text-brand-600"
                    >
                      <AssetLabel canonical={m.base} />
                    </Link>
                  </Td>
                  <Td>
                    <Link
                      href={`/markets/${encodeURIComponent(slug)}`}
                      className="hover:text-brand-600"
                    >
                      <AssetLabel canonical={m.quote} />
                    </Link>
                  </Td>
                  <Td align="right">
                    <LastPriceCell raw={m.last_price} />
                  </Td>
                  <Td align="right">
                    {vol != null && Number.isFinite(vol) && vol > 0 ? (
                      <span className="font-mono tabular-nums">
                        ${formatCompact(vol)}
                      </span>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right">
                    <span className="text-ink-body font-mono tabular-nums">
                      {m.trade_count_24h > 0
                        ? formatCompact(m.trade_count_24h)
                        : '0'}
                    </span>
                  </Td>
                  <Td align="right">
                    <span className="text-ink-muted font-mono text-xs tabular-nums">
                      {formatRelative(m.last_trade_at)}
                    </span>
                  </Td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      <div className="border-line flex items-center justify-between border-t px-4 py-2 text-xs">
        <button
          type="button"
          onClick={prevPage}
          disabled={!hasPrev}
          className="border-line text-ink-body hover:border-brand-500 hover:text-brand-600 rounded-md border px-3 py-1 disabled:cursor-not-allowed disabled:opacity-40"
        >
          ← Previous
        </button>
        <span className="text-ink-faint font-mono text-[11px]">
          page {pager.page}
        </span>
        <button
          type="button"
          onClick={nextPage}
          disabled={!hasNext}
          className="border-line text-ink-body hover:border-brand-500 hover:text-brand-600 rounded-md border px-3 py-1 disabled:cursor-not-allowed disabled:opacity-40"
        >
          Next →
        </button>
      </div>
    </Panel>
  );
}

function Th({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <th
      scope="col"
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </th>
  );
}

function Td({
  children,
  align,
}: {
  children: React.ReactNode;
  align?: 'left' | 'right';
}) {
  return (
    <td
      className={`px-4 py-2 ${align === 'right' ? 'text-right' : 'text-left'}`}
    >
      {children}
    </td>
  );
}
