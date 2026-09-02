'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { AssetLabel } from '@/components/AssetLabel';
import { type Envelope, apiGet, asExample } from '@/api/client';
import { formatCompact, formatRelative } from '@/lib/format';
import { sourceToneClass } from '@/lib/pillTone';
import { LastPriceCell } from '@/components/LastPriceCell';
import { useLedgerFollow } from '@/lib/live/hooks';
import {
  Button,
  Container,
  PageHeader,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';

import { DexProtocolsTable } from './DexProtocolsTable';
import { useCursorPager } from '@/lib/useCursorPager';
import { SortPill } from '@/components/SortPill';

interface Pool {
  source: string;
  base: string;
  quote: string;
  last_trade_at: string;
  trade_count_24h: number;
  volume_24h_usd?: string | null;
  last_price?: string | null;
}

type Order = 'volume_24h_usd_desc' | 'pair';

const PAGE_LIMIT = 100;

// Hard-coded to mirror internal/sources/external/registry.go's
// Subclass=DEX entries. Frontend doesn't have an /v1/sources?role=
// filter yet, so the pill row is static — keeps the chips visible
// before the first /v1/pools response lands.
const ALL_DEXES = ['aquarius', 'comet', 'phoenix', 'sdex', 'soroswap'];

// Source-name annotations that appear next to the source chip.
// Comet's only mainnet deployment is Blend's backstop pool —
// every Comet trade on Stellar is part of a liquidation auction,
// not retail price discovery. Surface that context inline so the
// row isn't read as a normal AMM venue. See
// docs/operations/wasm-audits/comet.md.
const SOURCE_NOTE: Record<string, string> = {
  comet: 'Blend backstop',
};

/**
 * DexesView — the all-pools explorer table. Same UX as /assets
 * (sortable header, paginated, drillable) but listing every
 * (source, base, quote) tuple in the trades store. Source filter
 * chips at the top let visitors scope the table to one venue.
 */
export function DexesView() {
  const [order, setOrder] = useState<Order>('volume_24h_usd_desc');
  const pager = useCursorPager();
  const { cursor } = pager;
  // Source filter is server-side. Empty string = all DEXes.
  const [sourceFilter, setSourceFilter] = useState<string>('');

  // Live (RT-2): follow ledger closes so the pools board + prices tick —
  // this was the one price table left static after the live-data sweep.
  useLedgerFollow(['/v1/pools']);
  const q = useQuery<{ pools: Pool[]; nextCursor?: string }>({
    queryKey: ['/v1/pools', order, cursor, sourceFilter],
    queryFn: async () => {
      const env = await apiGet<Envelope<Pool[]>>('/v1/pools', {
        order_by: order,
        limit: PAGE_LIMIT,
        ...(cursor ? { cursor } : {}),
        ...(sourceFilter ? { source: sourceFilter } : {}),
      });
      return {
        pools: env.data ?? [],
        nextCursor: env.pagination?.next,
      };
    },
  });

  const pools = q.data?.pools ?? [];

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
  function changeSource(next: string) {
    setSourceFilter(next);
    pager.reset();
  }

  const hasNext = !!q.data?.nextCursor;
  const hasPrev = pager.hasPrev;

  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="On-chain venues"
        title="DEXes"
        description={
          <>
            Every Stellar DEX we ingest — Soroswap, Phoenix, Aquarius, Comet,
            and the Stellar-native order book SDEX. The first table summarises
            each protocol; the second lists every (DEX, base, quote) pool
            we&apos;ve observed in the last 14 days. CEX trading pairs (Binance,
            Coinbase, Kraken, Bitstamp) live at{' '}
            <Link href="/exchanges" className="text-brand-600 hover:underline">
              /exchanges
            </Link>
            ; &ldquo;pool&rdquo; is AMM/DEX terminology.
          </>
        }
      />

      <DexProtocolsTable />

      <Panel
        headingLevel={2}
        title={`${pools.length} pools on this page${sourceFilter ? ` (${sourceFilter} only)` : ''}`}
        hint="Source: /v1/pools"
        source={asExample('/v1/pools', {
          limit: PAGE_LIMIT,
          order_by: order,
          ...(sourceFilter ? { source: sourceFilter } : {}),
        })}
        bodyClassName="-mx-4"
      >
        <div className="space-y-3 px-4 pt-1 pb-3">
          <div className="flex flex-wrap items-center gap-2 text-xs">
            <span className="text-ink-muted">Venue:</span>
            <SourceChip
              active={sourceFilter === ''}
              onClick={() => changeSource('')}
              label="All DEXes"
            />
            {ALL_DEXES.map((s) => (
              <SourceChip
                key={s}
                active={sourceFilter === s}
                onClick={() => changeSource(s)}
                label={s}
              />
            ))}
          </div>
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
              Source / pair (A→Z)
            </SortPill>
          </div>
        </div>

        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th>#</Th>
                <Th>Venue</Th>
                <Th>Base</Th>
                <Th>Quote</Th>
                <Th align="right">Last price</Th>
                <Th align="right">24h volume</Th>
                <Th align="right">24h trades</Th>
                <Th align="right">Last trade</Th>
              </tr>
            </THead>
            <TBody>
              {q.isLoading && !q.data && (
                <tr>
                  <td
                    colSpan={8}
                    className="text-ink-muted px-4 py-8 text-center text-sm"
                  >
                    Loading pools…
                  </td>
                </tr>
              )}
              {/* Distinguish "API failed" from "API returned 0 rows".
                  Pre-2026-05-13 a 503 from /v1/pools (the trades-
                  hypertable scan timed out) silently rendered as
                  "No pools matched." — QA finding F-02 in
                  docs/review-2026-05-13-live-site-qa.md. */}
              {!q.isLoading && q.isError && (
                <tr>
                  <td colSpan={8} className="px-4 py-8 text-center text-sm">
                    <div className="text-bad-700">
                      Couldn&apos;t load pools right now.
                    </div>
                    <div className="text-ink-muted mt-1 text-xs">
                      The pools query is timing out (likely a hot
                      trades-hypertable scan). Retry or check{' '}
                      <a
                        href="/status"
                        target="_blank"
                        rel="noopener noreferrer"
                        className="underline-offset-2 hover:underline"
                      >
                        the status page
                      </a>
                      .
                    </div>
                    <button
                      type="button"
                      onClick={() => q.refetch()}
                      className="border-bad-500/40 text-bad-700 hover:bg-bad-50 mt-2 rounded-md border px-3 py-1 text-xs"
                    >
                      Retry
                    </button>
                  </td>
                </tr>
              )}
              {!q.isLoading && !q.isError && pools.length === 0 && (
                <tr>
                  <td
                    colSpan={8}
                    className="text-ink-muted px-4 py-8 text-center text-sm"
                  >
                    No pools matched.
                  </td>
                </tr>
              )}
              {pools.map((p, i) => {
                const slug = `${p.base}~${p.quote}`;
                const offset = pager.depth * PAGE_LIMIT + i + 1;
                const vol = p.volume_24h_usd ? Number(p.volume_24h_usd) : null;
                const tone = sourceToneClass(p.source);
                return (
                  <TR key={`${p.source}|${p.base}|${p.quote}`}>
                    <Td>
                      <span className="text-ink-faint font-mono text-[11px]">
                        {offset}
                      </span>
                    </Td>
                    <Td>
                      <Link
                        href={`/dexes/${p.source}`}
                        className={`inline-block rounded-sm px-1.5 py-0.5 text-[10px] font-medium tracking-wider uppercase hover:underline ${tone}`}
                      >
                        {p.source}
                      </Link>
                      {SOURCE_NOTE[p.source] && (
                        <div className="text-ink-muted mt-0.5 text-[9px] tracking-wide uppercase">
                          {SOURCE_NOTE[p.source]}
                        </div>
                      )}
                    </Td>
                    <Td>
                      <Link
                        href={`/markets/${encodeURIComponent(slug)}`}
                        className="hover:text-brand-600"
                      >
                        <AssetLabel canonical={p.base} />
                      </Link>
                    </Td>
                    <Td>
                      <Link
                        href={`/markets/${encodeURIComponent(slug)}`}
                        className="hover:text-brand-600"
                      >
                        <AssetLabel canonical={p.quote} />
                      </Link>
                    </Td>
                    <Td align="right">
                      <LastPriceCell raw={p.last_price} />
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
                        {p.trade_count_24h > 0
                          ? formatCompact(p.trade_count_24h)
                          : '0'}
                      </span>
                    </Td>
                    <Td align="right">
                      <span className="text-ink-muted font-mono text-xs tabular-nums">
                        {formatRelative(p.last_trade_at)}
                      </span>
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </div>

        <div className="border-line flex items-center justify-between border-t px-4 py-3 text-xs">
          <Button
            variant="secondary"
            size="sm"
            onClick={prevPage}
            disabled={!hasPrev}
          >
            ← Previous
          </Button>
          <span className="text-ink-faint font-mono text-[11px]">
            page {pager.page}
          </span>
          <Button
            variant="secondary"
            size="sm"
            onClick={nextPage}
            disabled={!hasNext}
          >
            Next →
          </Button>
        </div>
      </Panel>

      <p className="text-ink-muted text-xs">
        Drill into a single DEX&apos;s pools at{' '}
        <Link href="/dexes/sdex" className="text-brand-600 hover:underline">
          /dexes/sdex
        </Link>
        ,{' '}
        <Link href="/dexes/soroswap" className="text-brand-600 hover:underline">
          /dexes/soroswap
        </Link>
        ,{' '}
        <Link href="/dexes/phoenix" className="text-brand-600 hover:underline">
          /dexes/phoenix
        </Link>
        ,{' '}
        <Link href="/dexes/aquarius" className="text-brand-600 hover:underline">
          /dexes/aquarius
        </Link>
        ,{' '}
        <Link href="/dexes/comet" className="text-brand-600 hover:underline">
          /dexes/comet
        </Link>
        .
      </p>
    </Container>
  );
}

function SourceChip({
  active,
  onClick,
  label,
}: {
  active: boolean;
  onClick: () => void;
  label: string;
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`rounded-full px-2 py-0.5 font-mono text-[10px] tracking-wider uppercase ${
        active
          ? 'bg-brand-600 text-white'
          : 'bg-surface-subtle text-ink-body hover:bg-line'
      }`}
    >
      {label}
    </button>
  );
}

// LastPriceCell moved to @/components/LastPriceCell (2026-08-21): this
// file's fork had ALSO silently dropped the flash-on-change every sibling
// table has — the exact drift COR-14/AGT-05 warned about. The shared cell
// restores it, and the useLedgerFollow above makes this board tick.
