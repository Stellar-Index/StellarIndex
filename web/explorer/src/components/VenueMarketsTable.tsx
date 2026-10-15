'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { useLedgerFollow } from '@/lib/live/hooks';
import { LastPriceCell } from '@/components/LastPriceCell';
import { Panel } from '@/components/reveal';
import { AssetLabel } from '@/components/AssetLabel';
import {
  Button,
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { type Envelope, apiGet, asExample } from '@/api/client';
import {
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
  formatRelative,
} from '@/lib/format';
// /v1/markets row from the generated OpenAPI contract, via the shared
// alias in src/api/hooks.ts (Market = MarketRow) — the contract-derived
// type wins: if /v1/markets changes, this build breaks loudly.
import type { Market } from '@/api/hooks';
import { useCursorPager } from '@/lib/useCursorPager';

type Order = 'volume_24h_usd_desc' | 'pair';

const PAGE_LIMIT = 100;

const ORDER_OPTIONS: { label: string; value: Order }[] = [
  { label: '24h volume', value: 'volume_24h_usd_desc' },
  { label: 'Pair, A to Z', value: 'pair' },
];

/**
 * VenueMarketsTable — every (base, quote) pair a venue observed in the
 * trailing 14d, from /v1/markets?source= with cursor pagination. That
 * filter refuses data vendors, so the venue must be an on-chain source or
 * an exchange. The caller supplies title + rowNoun ("SDEX markets" vs
 * pools; PairsTable supplies the exchange wording).
 */
export function VenueMarketsTable({
  source,
  title,
  rowNoun,
  headingLevel = 3,
}: {
  source: string;
  title: string;
  /** "pools" | "pairs" — loading/empty/unavailable copy. */
  rowNoun: string;
  /**
   * Heading rank for `title`, forwarded to `reveal/Panel`. Defaults to 3
   * (a panel nested under a section `<h2>`); a caller that renders the
   * table as a page's top-level section passes 2 so the outline doesn't
   * skip h2 (WCAG 1.3.1 — #486).
   */
  headingLevel?: 2 | 3 | 4;
}) {
  const [order, setOrder] = useState<Order>('volume_24h_usd_desc');
  const pager = useCursorPager();
  const { cursor } = pager;

  // Live (RT-2): follow ledger closes so this venue's pairs + prices tick.
  useLedgerFollow(['/v1/markets', source], undefined, pager.atTip);
  const q = useQuery<{ markets: Market[]; nextCursor?: string }>({
    queryKey: ['/v1/markets', source, order, cursor, 'venue-page'],
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
      headingLevel={headingLevel}
      title={title}
      hint="One row per (base, quote) pair observed in the last 14 days"
      source={asExample('/v1/markets', {
        source,
        order_by: order,
        limit: PAGE_LIMIT,
      })}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="mx-4 mb-4 flex flex-wrap items-center gap-3">
        <Segmented
          ariaLabel="Sort"
          options={ORDER_OPTIONS}
          value={order}
          onChange={(v) => changeOrder(v as Order)}
        />
        <span className="text-ink-muted tnum ml-auto text-xs">
          {markets ? `${markets.length} on this page` : '— on this page'}
          {q.isFetching && ' · refreshing…'}
        </span>
      </div>

      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th className="w-10">#</Th>
              <Th>Base</Th>
              <Th>Quote</Th>
              <Th align="right">Last price</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
              <Th align="right" className="hidden sm:table-cell">
                Last trade
              </Th>
            </tr>
          </THead>
          <TBody>
            {q.isLoading && !q.data && (
              <TR>
                <Td colSpan={7}>
                  <Skeleton className="h-5 w-64" />
                </Td>
              </TR>
            )}
            {!q.isLoading && !markets && (
              <TR>
                <Td colSpan={7} className="text-ink-muted">
                  {rowNoun === 'pools' ? 'Pool' : 'Pair'} list unavailable right
                  now — the markets query didn&apos;t return.
                  <Button
                    variant="secondary"
                    size="sm"
                    className="ml-3"
                    onClick={() => q.refetch()}
                  >
                    Retry
                  </Button>
                </Td>
              </TR>
            )}
            {!q.isLoading && markets && markets.length === 0 && (
              <TR>
                <Td colSpan={7} className="text-ink-muted">
                  No {rowNoun} found in the last 14 days.
                </Td>
              </TR>
            )}
            {rows.map((m, i) => {
              const slug = `${m.base}~${m.quote}`;
              const href = `/markets/${encodeURIComponent(slug)}`;
              const offset = pager.depth * PAGE_LIMIT + i + 1;
              const vol = m.volume_24h_usd;
              return (
                <LinkRow href={href} key={`${m.base}|${m.quote}`}>
                  <Td className="text-ink-faint font-mono tabular-nums">
                    {offset}
                  </Td>
                  <Td>
                    <Link href={href} className="text-ink hover:text-brand-600">
                      <AssetLabel canonical={m.base} />
                    </Link>
                  </Td>
                  <Td>
                    <Link
                      href={href}
                      className="text-ink-muted hover:text-brand-600"
                    >
                      <AssetLabel canonical={m.quote} />
                    </Link>
                  </Td>
                  <Td align="right">
                    <LastPriceCell raw={m.last_price} />
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {vol && compareDecimalStrings(vol, '0') === 1 ? (
                      `$${formatCompactUnits(vol)}`
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {m.trade_count_24h > 0
                      ? formatCompact(m.trade_count_24h)
                      : '0'}
                  </Td>
                  <Td
                    align="right"
                    className="text-ink-muted hidden font-mono text-xs tabular-nums sm:table-cell"
                    title={m.last_trade_at}
                  >
                    {formatRelative(m.last_trade_at)}
                  </Td>
                </LinkRow>
              );
            })}
          </TBody>
        </Table>
      </div>

      {(hasPrev || hasNext) && (
        <div className="border-line flex items-center justify-between border-t px-4 py-3">
          <span className="text-ink-muted tnum text-xs">Page {pager.page}</span>
          <div className="flex gap-2">
            <Button
              variant="secondary"
              size="sm"
              onClick={prevPage}
              disabled={!hasPrev}
            >
              Previous
            </Button>
            <Button
              variant="secondary"
              size="sm"
              onClick={nextPage}
              disabled={!hasNext}
            >
              Next
            </Button>
          </div>
        </div>
      )}
    </Panel>
  );
}
