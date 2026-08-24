'use client';

import Link from 'next/link';

import { useMarkets } from '@/api/hooks';
import { useLedgerFollow } from '@/lib/live/hooks';
import {
  EmptyState,
  Skeleton,
  Table,
  TableWrap,
  TBody,
  Td,
  Th,
  THead,
  TR,
} from '@/components/ui';
import { formatCompact, formatSubunitPrice } from '@/lib/format';
import { shortAssetText } from '@/lib/asset-label';

/**
 * HomeTopMarkets — top 10 trading pairs by trailing-24h USD
 * volume. Sits alongside the asset-centric panels on the home
 * page so a visitor sees *both* the most active assets and the
 * most-traded pairs without leaving the landing page.
 *
 * Pulls /v1/markets?limit=25&order_by=volume_24h_usd_desc — the
 * first page is enough to surface the top 10 with headroom, and
 * limit=25 hits the API's prewarmed cache key (the prewarm covers
 * limits 5/25/100/200, not 500). Pre-2026-05-09 this used
 * limit=500 and slugged the home page with a 5–8s cold-cache SQL
 * scan to throw away 490 rows. Each row deep-links to the per-pair
 * detail page at /markets/{base~quote} (PR #803).
 */
export function HomeTopMarkets() {
  const { data, isLoading, isError } = useMarkets(25, 'volume_24h_usd_desc');
  useLedgerFollow(['/v1/markets']);

  const top = (data?.markets ?? []).slice(0, 10);

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <div className="space-y-1">
          <h2 className="text-2xl font-semibold tracking-tight">Top markets</h2>
          <p className="text-ink-body text-sm">
            Pairs ranked by trailing-24h USD volume across all sources. Click a
            row for chart, recent trades, and per-source breakdown.
          </p>
        </div>
        <Link
          href="/markets"
          className="text-brand-600 text-xs hover:underline"
        >
          All markets →
        </Link>
      </div>
      {isError && top.length === 0 ? (
        <EmptyState
          title="Couldn't load top markets right now."
          action={
            <Link href="/markets" className="text-brand-600 hover:underline">
              Browse all markets →
            </Link>
          }
        />
      ) : (
        <TableWrap>
          <Table>
            <THead>
              <TR className="hover:bg-transparent">
                <Th>#</Th>
                <Th>Pair</Th>
                <Th align="right">Last price</Th>
                <Th align="right">24h volume</Th>
                <Th align="right">24h trades</Th>
              </TR>
            </THead>
            <TBody>
              {isLoading &&
                top.length === 0 &&
                Array.from({ length: 8 }).map((_, i) => (
                  <TR key={`sk-${i}`} className="hover:bg-transparent">
                    <Td colSpan={5}>
                      <Skeleton className="h-5 w-full" />
                    </Td>
                  </TR>
                ))}
              {top.map((m, i) => {
                const slug = `${m.base}~${m.quote}`;
                return (
                  <TR key={`${m.base}|${m.quote}`}>
                    <Td className="text-ink-faint">
                      <Link
                        href={`/markets/${encodeURIComponent(slug)}`}
                        className="hover:text-brand-600"
                      >
                        {i + 1}
                      </Link>
                    </Td>
                    <Td>
                      <Link
                        href={`/markets/${encodeURIComponent(slug)}`}
                        className="text-ink hover:text-brand-600 font-medium"
                      >
                        {shortAssetText(m.base)}
                        <span className="text-ink-faint mx-1">/</span>
                        {shortAssetText(m.quote)}
                      </Link>
                    </Td>
                    <Td align="right" className="text-ink-body font-mono">
                      {m.last_price ? formatLastPrice(m.last_price) : '—'}
                    </Td>
                    <Td align="right" className="font-mono">
                      {m.volume_24h_usd
                        ? `$${formatCompact(Number(m.volume_24h_usd))}`
                        : '—'}
                    </Td>
                    <Td align="right" className="text-ink-body font-mono">
                      {formatCompact(m.trade_count_24h)}
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

function formatLastPrice(raw: string): string {
  const n = Number(raw);
  if (!Number.isFinite(n)) return '—';
  return n >= 1000
    ? n.toFixed(2)
    : n >= 1
      ? n.toFixed(4)
      : n >= 0.0001
        ? n.toFixed(6)
        : formatSubunitPrice(n);
}
