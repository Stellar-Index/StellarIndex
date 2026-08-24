'use client';

import { useMemo } from 'react';
import { keepPreviousData, useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { useMarkets } from '@/api/hooks';
import { useLedgerFollow } from '@/lib/live/hooks';
import { apiGet } from '@/api/client';
import {
  EmptyState,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';

// /v1/history row from the generated OpenAPI contract, via the shared
// alias in src/api/hooks.ts (TradeRow).
import type { TradeRow as Trade } from '@/api/hooks';
import { formatRelative } from '@/lib/format';
import { shortAssetText } from '@/lib/asset-label';

const REFRESH_INTERVAL_MS = 30_000;
// Minimum gap between stream-nudged refetches. Ledgers close every ~5s;
// coalescing to 10s keeps the feed visibly ledger-driven while capping
// the fan-out at 3 pairs × 6 req/min.
const STREAM_COALESCE_MS = 10_000;
const TOP_PAIRS = 3;
const PER_PAIR_LIMIT = 12;
const DISPLAY_LIMIT = 30;

/**
 * HomeRecentTrades — rolling feed of the most recent trades
 * across the top-3 pairs by 24h volume. Pulls from
 * /v1/markets to enumerate the pairs, then fans out to
 * /v1/history?base=…&quote=… for each. Merged client-side by
 * `ts desc` and rendered as a table.
 *
 * Live follow (RT-2): ledger closes from the shared SSE stream nudge
 * the fan-out (coalesced to STREAM_COALESCE_MS) so new trades land
 * within seconds of their ledger — one connection, shared with every
 * other stream consumer on the page, never per-pair (the per-IP
 * stream-cap rationale from MarketsTable). The 30s interval stays as
 * the fallback when the stream is unavailable. The merge cap keeps
 * the panel a fixed height regardless of fan-out depth.
 */
export function HomeRecentTrades() {
  const markets = useMarkets(TOP_PAIRS, 'volume_24h_usd_desc');

  // Fan out per pair, merge by ts desc, take the top DISPLAY_LIMIT.
  const pairs = useMemo(
    () =>
      (markets.data?.markets ?? [])
        .slice(0, TOP_PAIRS)
        .map((m) => ({ base: m.base, quote: m.quote })),
    [markets.data],
  );

  // FEC audit A6-4: the fan-out is a react-query query and the stream
  // nudge is the canonical useLedgerFollow — the previous hand-rolled
  // generation-counter/last-poll machinery re-implemented exactly what
  // the hook + query cache own. The 10s coalesce (documented deliberate
  // above) survives as the hook's minIntervalMs; the 30s fallback is
  // refetchInterval.
  useLedgerFollow(['home-recent-trades'], STREAM_COALESCE_MS);
  const q = useQuery<Trade[]>({
    queryKey: ['home-recent-trades', pairs],
    enabled: pairs.length > 0,
    refetchInterval: REFRESH_INTERVAL_MS,
    placeholderData: keepPreviousData,
    queryFn: async () => {
      const fanouts = await Promise.all(
        pairs.map((p) =>
          apiGet<Trade[]>('/v1/history', {
            base: p.base,
            quote: p.quote,
            limit: PER_PAIR_LIMIT,
          }),
        ),
      );
      return fanouts
        .flat()
        .sort((a, b) => (a.ts < b.ts ? 1 : -1))
        .slice(0, DISPLAY_LIMIT);
    },
  });
  const trades = q.data ?? [];
  const error = q.isError
    ? q.error instanceof Error
      ? q.error.message
      : 'Network error'
    : null;

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <div className="space-y-1">
          <h2 className="flex items-center gap-2 text-2xl font-semibold tracking-tight">
            Recent trades
            <span
              className="relative inline-flex h-2 w-2"
              aria-label="live feed"
              title="live feed"
            >
              <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75"></span>
              <span className="bg-up relative inline-flex h-2 w-2 rounded-full"></span>
            </span>
          </h2>
          <p className="text-ink-body text-sm">
            Live feed merging the latest trades across the top {TOP_PAIRS} pairs
            by 24h USD volume. Ticks with ledger closes.
          </p>
        </div>
      </div>
      {error && (
        <div className="rounded-card border-down-subtle bg-down-subtle/30 text-down border px-4 py-2 text-xs">
          Live feed unreachable: {error}
        </div>
      )}
      {trades.length === 0 ? (
        <EmptyState title="Waiting for first trades…" />
      ) : (
        <TableWrap className="max-h-96 overflow-y-auto">
          <Table>
            <THead className="sticky top-0 z-10">
              <TR className="hover:bg-transparent">
                <Th>Time</Th>
                <Th>Pair</Th>
                <Th>Source</Th>
                <Th align="right">Price</Th>
              </TR>
            </THead>
            <TBody className="font-mono text-xs">
              {trades.map((t, i) => {
                // Both sides need to be defined to construct a
                // valid /markets/<base~quote> route. If either
                // is null (rare — see comment in `shortAssetText()`), we
                // render the row but don't link it; sending the
                // user to /markets/native~undefined would 404.
                const linkable = !!t.base_asset && !!t.quote_asset;
                const slug = linkable ? `${t.base_asset}~${t.quote_asset}` : '';
                const pairLabel = (
                  <>
                    {shortAssetText(t.base_asset)} /{' '}
                    {shortAssetText(t.quote_asset)}
                  </>
                );
                return (
                  <TR key={`${t.ts}-${t.source}-${i}`}>
                    <Td className="text-ink-muted tabular-nums">
                      {t.tx_hash ? (
                        <a
                          href={`https://stellar.expert/explorer/public/tx/${t.tx_hash}`}
                          target="_blank"
                          rel="noreferrer noopener"
                          className="hover:text-brand-600 hover:underline"
                          title={`View tx ${t.tx_hash} on stellar.expert`}
                        >
                          {formatRelative(t.ts, { suffix: false })}
                        </a>
                      ) : (
                        formatRelative(t.ts, { suffix: false })
                      )}
                    </Td>
                    <Td>
                      {linkable ? (
                        <Link
                          href={`/markets/${encodeURIComponent(slug)}`}
                          className="hover:text-brand-600"
                        >
                          {pairLabel}
                        </Link>
                      ) : (
                        <span>{pairLabel}</span>
                      )}
                    </Td>
                    <Td className="text-ink-body tracking-wider uppercase">
                      <Link
                        href={`/sources/${t.source}`}
                        className="hover:text-brand-600"
                      >
                        {t.source}
                      </Link>
                    </Td>
                    <Td align="right" className="tabular-nums">
                      {t.price}
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </TableWrap>
      )}
    </section>
  );
}
