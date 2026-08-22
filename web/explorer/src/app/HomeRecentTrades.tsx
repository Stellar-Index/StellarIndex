'use client';

import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import Link from 'next/link';

import { useMarkets } from '@/api/hooks';
import { useLedgerStream } from '@/lib/live/hooks';
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
  const [trades, setTrades] = useState<Trade[]>([]);
  const [error, setError] = useState<string | null>(null);

  // Fan out per pair, merge by ts desc, take the top DISPLAY_LIMIT.
  const pairs = useMemo(
    () =>
      (markets.data?.markets ?? [])
        .slice(0, TOP_PAIRS)
        .map((m) => ({ base: m.base, quote: m.quote })),
    [markets.data],
  );

  // Generation counter invalidates in-flight polls when the pair set
  // changes or the component unmounts (the old closure-`cancelled`
  // flag, but shareable across the two trigger effects below).
  const genRef = useRef(0);
  const lastPollRef = useRef(0);

  const poll = useCallback(async () => {
    if (pairs.length === 0) return;
    const gen = genRef.current;
    lastPollRef.current = Date.now();
    try {
      const fanouts = await Promise.all(
        pairs.map((p) =>
          apiGet<Trade[]>('/v1/history', {
            base: p.base,
            quote: p.quote,
            limit: PER_PAIR_LIMIT,
          }),
        ),
      );
      if (gen !== genRef.current) return;
      const merged = fanouts
        .flat()
        .sort((a, b) => (a.ts < b.ts ? 1 : -1))
        .slice(0, DISPLAY_LIMIT);
      setTrades(merged);
      setError(null);
    } catch (e) {
      if (gen !== genRef.current) return;
      setError(e instanceof Error ? e.message : 'Network error');
    }
  }, [pairs]);

  // Fallback cadence: poll now (next tick — keeps the effect body free
  // of state writes for the react-hooks compiler rule) + every
  // REFRESH_INTERVAL_MS. The generation bump at effect start invalidates
  // any in-flight poll from a previous pair set; on unmount the stale
  // poll's setState is a React-18 no-op, so no cleanup bump is needed.
  useEffect(() => {
    genRef.current++;
    if (pairs.length === 0) return;
    const kick = setTimeout(() => void poll(), 0);
    const id = setInterval(() => void poll(), REFRESH_INTERVAL_MS);
    return () => {
      clearTimeout(kick);
      clearInterval(id);
    };
  }, [pairs, poll]);

  // Stream nudge: a ledger close means possible new trades — refetch
  // sooner than the fallback, coalesced.
  const frame = useLedgerStream();
  const streamLatest = frame?.data.latest_ledger;
  useEffect(() => {
    if (streamLatest == null || pairs.length === 0) return;
    if (Date.now() - lastPollRef.current < STREAM_COALESCE_MS) return;
    void poll();
  }, [streamLatest, pairs, poll]);

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
              <span className="relative inline-flex h-2 w-2 rounded-full bg-up"></span>
            </span>
          </h2>
          <p className="text-sm text-ink-body">
            Live feed merging the latest trades across the top {TOP_PAIRS}{' '}
            pairs by 24h USD volume. Ticks with ledger closes.
          </p>
        </div>
      </div>
      {error && (
        <div className="rounded-card border border-down-subtle bg-down-subtle/30 px-4 py-2 text-xs text-down">
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
                // is null (rare — see comment in `short()`), we
                // render the row but don't link it; sending the
                // user to /markets/native~undefined would 404.
                const linkable = !!t.base_asset && !!t.quote_asset;
                const slug = linkable ? `${t.base_asset}~${t.quote_asset}` : '';
                const pairLabel = (
                  <>
                    {short(t.base_asset)} / {short(t.quote_asset)}
                  </>
                );
                return (
                  <TR key={`${t.ts}-${t.source}-${i}`}>
                    <Td className="tabular-nums text-ink-muted">
                      {t.tx_hash ? (
                        <a
                          href={`https://stellar.expert/explorer/public/tx/${t.tx_hash}`}
                          target="_blank"
                          rel="noreferrer noopener"
                          className="hover:text-brand-600 hover:underline"
                          title={`View tx ${t.tx_hash} on stellar.expert`}
                        >
                          {timeAgo(t.ts)}
                        </a>
                      ) : (
                        timeAgo(t.ts)
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
                    <Td className="uppercase tracking-wider text-ink-body">
                      <Link href={`/sources/${t.source}`} className="hover:text-brand-600">
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

function short(canonical: string | undefined | null): string {
  // /v1/history rows occasionally arrive with one side null (e.g.
  // a trade whose decoder couldn't resolve the counterparty asset
  // — the row still surfaces with the price + timestamp because
  // those came from elsewhere). Guard so an undefined doesn't
  // crash the whole feed render.
  if (!canonical) return '—';
  if (canonical === 'native') return 'XLM';
  if (canonical.startsWith('fiat:')) return canonical.replace('fiat:', '');
  if (canonical.startsWith('crypto:')) return canonical.replace('crypto:', '');
  const dashIx = canonical.indexOf('-');
  if (dashIx === -1) return canonical;
  return canonical.slice(0, dashIx);
}

function timeAgo(iso: string): string {
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '—';
  const s = Math.round(ms / 1000);
  if (s < 0) return 'now';
  if (s < 60) return `${s}s`;
  const m = Math.round(s / 60);
  if (m < 60) return `${m}m`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h}h`;
  return `${Math.round(h / 24)}d`;
}
