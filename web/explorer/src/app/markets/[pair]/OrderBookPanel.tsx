'use client';

import { useQuery } from '@tanstack/react-query';

import { apiGet } from '@/api/client';
import type { components } from '@/api/types';
import {
  DepthChart,
  OrderBookStatStrip,
  computeBookStats,
} from '@/components/charts/DepthChart';
import { shortAssetText } from '@/lib/asset-label';

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
    <section className="border-line bg-surface rounded-lg border p-4">
      <header className="mb-3 flex items-baseline justify-between">
        <h2 className="text-ink-body text-sm font-semibold tracking-wider uppercase">
          SDEX order book
        </h2>
        {book && (
          <span className="text-ink-faint text-xs">
            as of ledger {book.as_of_ledger.toLocaleString('en-US')}
          </span>
        )}
      </header>

      {q.isLoading && (
        <p className="text-ink-muted text-sm">Loading order book…</p>
      )}
      {loading503 && (
        <p className="text-ink-muted text-sm">
          The order-book snapshot is still loading after a server restart —
          check back shortly.
        </p>
      )}
      {q.isError && !loading503 && (
        <p className="text-ink-muted text-sm">Order book unavailable.</p>
      )}
      {empty && (
        <p className="text-ink-muted text-sm">
          No live SDEX offers on this pair right now.
        </p>
      )}

      {book && !empty && (
        <div className="space-y-4">
          {/* Cumulative depth — the flagship trader visual: how much
              base-asset size sits between here and N bps of slippage.
              Levels come straight from the served book's
              cum_base_amount/cum_quote_amount; the strip's spread/mid
              are derived from the top-of-book and refuse a crossed
              snapshot rather than printing a negative spread. */}
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
    </section>
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
  const barClass = tone === 'up' ? 'bg-up-subtle' : 'bg-warn-50';
  return (
    <div>
      <div className="text-ink-muted mb-1 flex items-baseline justify-between text-[10px] tracking-wider uppercase">
        <span>{side}</span>
        <span className="text-ink-faint">
          {truncated > levels.reduce((n, l) => n + l.offers, 0)
            ? `top ${levels.length} levels`
            : `${truncated} offers`}
        </span>
      </div>
      <table className="min-w-full text-xs">
        <thead>
          <tr className="text-ink-muted text-[10px] tracking-wider uppercase">
            <th className="py-1 text-left font-medium">Price</th>
            <th className="py-1 text-right font-medium">Amount</th>
            <th className="py-1 text-right font-medium">Total</th>
          </tr>
        </thead>
        <tbody>
          {levels.map((l) => {
            const pct =
              maxCum > 0
                ? Math.min(100, (Number(l.cum_base_amount) / maxCum) * 100)
                : 0;
            return (
              <tr key={`${l.price_r.n}/${l.price_r.d}`} className="relative">
                <td className="relative py-0.5 pr-2 font-mono tabular-nums">
                  <span
                    aria-hidden
                    className={`absolute inset-y-0 left-0 ${barClass}`}
                    style={{ width: `${pct}%` }}
                  />
                  <span className="relative">{trimTrailingZeros(l.price)}</span>
                </td>
                <td className="relative py-0.5 text-right font-mono tabular-nums">
                  {trimTrailingZeros(l.base_amount)}
                </td>
                <td className="text-ink-muted relative py-0.5 text-right font-mono tabular-nums">
                  {trimTrailingZeros(l.cum_base_amount)}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** trimTrailingZeros — "10.5000000" → "10.5", "0.5000000" → "0.5". */
export function trimTrailingZeros(s: string): string {
  if (!s.includes('.')) return s;
  return s.replace(/\.?0+$/, '') || '0';
}
