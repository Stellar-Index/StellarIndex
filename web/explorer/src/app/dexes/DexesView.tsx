'use client';

import { useState } from 'react';
import { useQuery } from '@tanstack/react-query';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { AssetLabel } from '@/components/AssetLabel';
import { type Envelope, apiGet, asExample } from '@/api/client';
import {
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
  formatRelative,
} from '@/lib/format';
import { LastPriceCell } from '@/components/LastPriceCell';
import { useLedgerFollow } from '@/lib/live/hooks';
import {
  Button,
  Container,
  PageHeader,
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { protocolMeta } from '@/app/protocols/registry';
import { DexProtocolsTable } from './DexProtocolsTable';
import { useCursorPager } from '@/lib/useCursorPager';
import { hrefFor } from '@/lib/hrefFor';

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
// filter yet, so the venue control is static — it renders before the
// first /v1/pools response lands.
//
// scripts/ci/lint-protocol-registry-sync.sh §2 fails on any drift
// between this list, DEX_INFO in dexes/registry.ts and the Go
// registry, so a new DEX cannot ship with no filter entry and no page.
const ALL_DEXES = [
  'aquarius',
  'comet',
  'phoenix',
  'sdex',
  'soroswap',
  'sushiswap_v3',
];

// Source-name annotations that appear next to the venue name.
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
 * (source, base, quote) tuple in the trades store. A venue control
 * at the top scopes the table to one DEX.
 */
export function DexesView() {
  const [order, setOrder] = useState<Order>('volume_24h_usd_desc');
  const pager = useCursorPager();
  const { cursor } = pager;
  // Source filter is server-side. Empty string = all DEXes.
  const [sourceFilter, setSourceFilter] = useState<string>('');

  // Live (RT-2): follow ledger closes so the pools board + prices tick —
  // this was the one price table left static after the live-data sweep.
  useLedgerFollow(['/v1/pools'], undefined, pager.atTip);
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

  const venueOptions = [
    { label: 'All venues', value: '' },
    ...ALL_DEXES.map((d) => ({ label: protocolMeta(d)?.label ?? d, value: d })),
  ];
  const orderOptions: { label: string; value: Order }[] = [
    { label: '24h volume', value: 'volume_24h_usd_desc' },
    { label: 'Pair, A to Z', value: 'pair' },
  ];

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'DEXes' }]}
        title="DEXes"
        meta="Every on-chain venue we index, then every pool across them. Volumes and counts cover the last 24 hours."
      />
      <DexProtocolsTable />
      <Panel
        headingLevel={2}
        title="Pools"
        hint={
          q.isLoading
            ? 'Loading pools'
            : `${pools.length} pools on this page · ${PAGE_LIMIT} per page`
        }
        source={asExample('/v1/pools', {
          order_by: order,
          limit: PAGE_LIMIT,
          ...(sourceFilter ? { source: sourceFilter } : {}),
        })}
        bodyClassName="-mx-4 -mb-4"
      >
        <div className="mx-4 mb-4 flex flex-wrap items-center gap-3">
          <Segmented
            ariaLabel="Venue"
            className="flex-wrap"
            options={venueOptions}
            value={sourceFilter}
            onChange={changeSource}
          />
          <Segmented
            ariaLabel="Sort"
            options={orderOptions}
            value={order}
            onChange={(v) => changeOrder(v as Order)}
          />
        </div>
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th className="w-10">#</Th>
                <Th>Venue</Th>
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
              {q.isLoading && (
                <TR>
                  <Td colSpan={8}>
                    <Skeleton className="h-5 w-64" />
                  </Td>
                </TR>
              )}
              {q.isError && (
                <TR>
                  <Td colSpan={8} className="text-ink-muted">
                    Pool list unavailable right now.{' '}
                    <Link
                      href="/status"
                      className="text-brand-600 hover:underline"
                    >
                      Status
                    </Link>
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
              {!q.isLoading && !q.isError && pools.length === 0 && (
                <TR>
                  <Td colSpan={8} className="text-ink-muted">
                    No pools match this filter.
                  </Td>
                </TR>
              )}
              {pools.map((p, i) => {
                const slug = `${p.base}~${p.quote}`;
                const href = `/markets/${encodeURIComponent(slug)}`;
                const vol = p.volume_24h_usd;
                return (
                  <TR key={`${p.source}:${slug}`}>
                    <Td className="text-ink-faint font-mono tabular-nums">
                      {pager.depth * PAGE_LIMIT + i + 1}
                    </Td>
                    <Td>
                      <Link
                        href={hrefFor.dex(p.source)}
                        className="text-ink-muted hover:text-brand-600"
                      >
                        {protocolMeta(p.source)?.label ?? p.source}
                      </Link>
                      {SOURCE_NOTE[p.source] && (
                        <span className="text-ink-faint ml-1.5 text-xs">
                          {SOURCE_NOTE[p.source]}
                        </span>
                      )}
                    </Td>
                    <Td>
                      <Link
                        href={href}
                        className="text-ink hover:text-brand-600"
                      >
                        <AssetLabel canonical={p.base} />
                      </Link>
                    </Td>
                    <Td>
                      <Link
                        href={href}
                        className="text-ink-muted hover:text-brand-600"
                      >
                        <AssetLabel canonical={p.quote} />
                      </Link>
                    </Td>
                    <Td align="right">
                      <LastPriceCell raw={p.last_price} />
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {vol && compareDecimalStrings(vol, '0') === 1 ? (
                        `$${formatCompactUnits(vol)}`
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {formatCompact(p.trade_count_24h)}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted hidden font-mono text-xs tabular-nums sm:table-cell"
                      title={p.last_trade_at}
                    >
                      {formatRelative(p.last_trade_at)}
                    </Td>
                  </TR>
                );
              })}
            </TBody>
          </Table>
        </div>
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
      </Panel>
      <p className="text-ink-muted text-sm">
        Each venue has its own page:{' '}
        {ALL_DEXES.map((d, i) => (
          <span key={d}>
            {i > 0 && ' · '}
            <Link
              href={hrefFor.dex(d)}
              className="text-brand-600 hover:underline"
            >
              {protocolMeta(d)?.label ?? d}
            </Link>
          </span>
        ))}
        .
      </p>
    </Container>
  );
}
