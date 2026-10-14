'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet, asExample } from '@/api/client';
import type { components } from '@/api/types';
import { Panel } from '@/components/reveal';
import {
  Callout,
  EmptyState,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { cn } from '@/lib/cn';
import { shortAssetText } from '@/lib/asset-label';
import { formatReadable } from '@/lib/format';
import { DepthChart, OrderBookStatStrip, computeBookStats } from './DepthChart';

type OrderBook = components['schemas']['SDEXOrderBook'];
type Level = components['schemas']['SDEXOrderBookLevel'];

/**
 * isClassicAssetId — the SDEX book only exists for classic asset
 * forms (`native` / `CODE-G...`); Soroban `C...` pairs have no
 * central order book, so the panel renders nothing for them.
 */
export function isClassicAssetId(id: string): boolean {
  return id === 'native' || /^[A-Za-z0-9]{1,12}-G[A-Z2-7]{55}$/.test(id);
}

const DEPTH = 12;

/**
 * OrderBookPanel — live SDEX bid/ask depth for a classic pair, from
 * GET /v1/sdex/orderbook (an in-process book advanced ~60s server-
 * side). Amounts/prices arrive as exact decimal strings; depth bars
 * scale by cumulative base amount. Honest states: while the server
 * snapshot is loading (503) the panel says so; a pair with no live
 * offers renders an explicit empty state, and non-classic pairs render
 * nothing at all.
 */
export function OrderBookPanel({
  base,
  quote,
}: {
  base: string;
  quote: string;
}) {
  const classic = isClassicAssetId(base) && isClassicAssetId(quote);
  const q = useQuery<OrderBook>({
    queryKey: ['/v1/sdex/orderbook', base, quote],
    queryFn: async () => {
      const env = await apiGet<{ data: OrderBook }>('/v1/sdex/orderbook', {
        selling: base,
        buying: quote,
        depth: String(DEPTH),
      });
      return env.data;
    },
    enabled: classic,
    refetchInterval: 60_000,
    retry: false,
  });

  if (!classic) return null;

  const loading503 = q.isError && /503/.test((q.error as Error)?.message ?? '');
  const book = q.data;
  const empty = book && book.asks.length === 0 && book.bids.length === 0;

  return (
    <Panel
      headingLevel={2}
      title="SDEX order book"
      hint={
        book
          ? `as of ledger ${book.as_of_ledger.toLocaleString('en-US')}`
          : 'Live bid and ask depth on the Stellar DEX'
      }
      source={asExample('/v1/sdex/orderbook', {
        selling: base,
        buying: quote,
        depth: DEPTH,
      })}
    >
      {q.isLoading && <Skeleton className="h-64 w-full" />}
      {loading503 && (
        <Callout tone="warn">
          The order-book snapshot is still loading after a server restart; check
          back shortly.
        </Callout>
      )}
      {q.isError && !loading503 && (
        <Callout tone="bad">Order book unavailable.</Callout>
      )}
      {empty && (
        <EmptyState
          title="No live SDEX offers"
          description="Nobody is quoting this pair on the Stellar DEX right now."
        />
      )}

      {book && !empty && (
        <div className="space-y-4">
          {/* Cumulative depth: how much base-asset size sits between here
              and N bps of slippage. Levels come straight from the served
              book's cum_base_amount/cum_quote_amount; the strip's
              spread/mid are derived from the top-of-book and refuse a
              crossed snapshot rather than printing a negative spread. */}
          <OrderBookStatStrip
            stats={computeBookStats(book.bids, book.asks)}
            quoteLabel={shortAssetText(book.buying)}
          />
          <DepthChart
            bids={book.bids}
            asks={book.asks}
            baseLabel={shortAssetText(book.selling)}
            quoteLabel={shortAssetText(book.buying)}
          />
          <div className="grid gap-4 sm:grid-cols-2">
            <DepthTable
              side="Bids"
              levels={book.bids}
              tone="up"
              truncated={book.bid_offers}
            />
            <DepthTable
              side="Asks"
              levels={book.asks}
              tone="down"
              truncated={book.ask_offers}
            />
          </div>
        </div>
      )}
    </Panel>
  );
}

function DepthTable({
  side,
  levels,
  tone,
  truncated,
}: {
  side: string;
  levels: Level[];
  tone: 'up' | 'down';
  truncated: number;
}) {
  const maxCum =
    levels.length > 0 ? Number(levels[levels.length - 1].cum_base_amount) : 0;
  const barClass = tone === 'up' ? 'bg-up-subtle' : 'bg-down-subtle';
  const shownOffers = levels.reduce((n, l) => n + l.offers, 0);
  return (
    <div>
      <div className="mb-2 flex items-baseline justify-between">
        <h3
          className={cn(
            PANEL_LABEL_CLASS,
            tone === 'up' ? 'text-up' : 'text-down',
          )}
        >
          {side}
        </h3>
        <span className="text-ink-muted tnum text-xs">
          {truncated > shownOffers
            ? `top ${levels.length} levels`
            : `${truncated} offers`}
        </span>
      </div>
      {/* WCAG 1.4.10 Reflow: the mono price/amount columns scroll in
          place instead of pushing the page sideways at 320px. */}
      <div className="overflow-x-auto">
        <Table className="text-xs">
          <THead>
            <tr>
              <Th align="right" className="h-8 px-2">
                Price
              </Th>
              <Th align="right" className="h-8 px-2">
                Amount
              </Th>
              <Th align="right" className="h-8 px-2">
                Total
              </Th>
            </tr>
          </THead>
          <TBody>
            {levels.map((l) => {
              const pct =
                maxCum > 0
                  ? Math.min(100, (Number(l.cum_base_amount) / maxCum) * 100)
                  : 0;
              return (
                <TR key={`${l.price_r.n}/${l.price_r.d}`}>
                  <Td
                    align="right"
                    className="relative px-2 py-1 font-mono"
                    title={l.price}
                  >
                    <span
                      aria-hidden
                      className={cn('absolute inset-y-0 left-0', barClass)}
                      style={{ width: `${pct}%` }}
                    />
                    <span className="relative">{fixedPlaces(l.price, 7)}</span>
                  </Td>
                  <Td
                    align="right"
                    className="px-2 py-1 font-mono"
                    title={l.base_amount}
                  >
                    {formatReadable(l.base_amount) ?? l.base_amount}
                  </Td>
                  <Td
                    align="right"
                    className="text-ink-muted px-2 py-1 font-mono"
                    title={l.cum_base_amount}
                  >
                    {formatReadable(l.cum_base_amount) ?? l.cum_base_amount}
                  </Td>
                </TR>
              );
            })}
          </TBody>
        </Table>
      </div>
    </div>
  );
}

/** fixedPlaces — pads or cuts a decimal string to `n` places so a column of
 * prices shares one decimal point; the exact value stays in the title. */
export function fixedPlaces(s: string, n: number): string {
  const [whole, frac = ''] = s.split('.');
  return `${whole}.${(frac + '0'.repeat(n)).slice(0, n)}`;
}

/** trimTrailingZeros — "10.5000000" → "10.5", "0.5000000" → "0.5". */
export function trimTrailingZeros(s: string): string {
  if (!s.includes('.')) return s;
  return s.replace(/\.?0+$/, '') || '0';
}
