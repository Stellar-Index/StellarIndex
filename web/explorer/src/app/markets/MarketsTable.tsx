'use client';

import { useEffect, useMemo, useRef, useState } from 'react';
import Link from 'next/link';
import { useRouter, useSearchParams } from 'next/navigation';
import { useQueryClient } from '@tanstack/react-query';
import { Search } from 'lucide-react';

import { Panel } from '@/components/reveal';
import { RequestReveal } from '@/components/reveal/RequestReveal';
import { asExample } from '@/api/client';
import { AssetLabel } from '@/components/AssetLabel';
import { SourceSparkline } from '@/components/SourceSparkline';
import { VolumeShare, topWithOther } from '@/components/charts/VolumeShare';
import { shortAssetText } from '@/lib/asset-label';
import { useMarkets } from '@/api/hooks';
import { cn } from '@/lib/cn';
import {
  formatCompact,
  formatCompactUnits,
  formatRelative,
} from '@/lib/format';
import { useLedgerStream } from '@/lib/live/hooks';
import { LastPriceCell } from '@/components/LastPriceCell';
import { FreshnessMarker } from '@/components/primitives';
import { SortableTh, type SortState } from '@/lib/useTableSort';
import {
  ButtonLink,
  Callout,
  EmptyState,
  Input,
  Segmented,
  Skeleton,
  TBody,
  TR,
  Table,
  TableWrap,
  Td,
  Th,
  THead,
} from '@/components/ui';

type Order = 'volume_24h_usd_desc' | 'pair';
type SortKey = 'pair' | 'volume';
type Venue = 'stellar' | 'reference' | 'all';

const VENUE_OPTIONS: { value: Venue; label: string }[] = [
  { value: 'stellar', label: 'On Stellar' },
  { value: 'reference', label: 'Reference feeds' },
  { value: 'all', label: 'All' },
];

// Column priority: pair, price and volume always show; the rest return as
// the viewport widens.
const COL = {
  sm: 'hidden sm:table-cell',
  md: 'hidden md:table-cell',
  lg: 'hidden lg:table-cell',
};

/** On-Stellar when the asset is native, classic CODE-G…/CODE:G… or a
 * C-address; crypto:/fiat: forms are off-chain reference feeds. */
function isStellarAsset(a: string): boolean {
  return (
    a === 'native' ||
    /^C[A-Z2-7]{55}$/.test(a) ||
    /[:-]G[A-Z2-7]{55}$/.test(a) ||
    /^\d+$/.test(a)
  );
}

/** Minimum gap between live refetches of /v1/markets. Ledgers close
 * every ~5s but the markets directory is a heavier read than a tip
 * price — one refresh per ~15s keeps the board feeling live without
 * tripling the endpoint's load. */
const MARKETS_LIVE_REFRESH_MS = 15_000;

/**
 * Live-data markets table backed by `/v1/markets`.
 *
 * Default sort is `volume_24h_usd_desc` so the high-activity pairs
 * land in the first page. Click the 24h volume header to toggle
 * back to the alphabetical-by-pair sort (the API's `pair` order_by);
 * URL ?order= preserves the choice across navigation.
 *
 * Per ADR-0015 the API returns "active markets" only (pairs that
 * traded in the last 14 days). Cursor pagination is plumbed through
 * the hook but the v0 page only shows the first 100; "Load more"
 * lands once we add virtual scrolling.
 */
export function MarketsTable() {
  const router = useRouter();
  const params = useSearchParams();
  const orderParam = params.get('order') ?? '';
  // Default to volume desc (high-activity first); ?order=pair flips
  // to the API's alphabetical-by-pair order. Anything else falls
  // back to the default.
  const orderBy: 'volume_24h_usd_desc' | 'pair' =
    orderParam === 'pair' ? 'pair' : 'volume_24h_usd_desc';
  // ?asset=<canonical id or verified slug> restricts to pairs where the
  // asset appears on either side (server-side filter — see markets.go).
  // This is the deep-link target for "all markets for this asset" from
  // the asset + issuer pages.
  const assetParam = (params.get('asset') ?? '').trim();

  const { data, isLoading, isError, error } = useMarkets(100, orderBy, {
    sparkline: true,
    ...(assetParam ? { asset: assetParam } : {}),
  });

  // Live follow (RT-2): ledger closes from the shared stream drive a
  // throttled refetch of the markets board — ONE SSE connection for
  // the whole table (a per-pair tip stream here would burn the per-IP
  // stream cap on one page). Changed last-price cells flash via
  // LastPriceCell's own tick detector.
  const frame = useLedgerStream();
  const queryClient = useQueryClient();
  const lastRefetchRef = useRef(0);
  const streamLatest = frame?.data.latest_ledger;
  useEffect(() => {
    if (streamLatest == null) return;
    const now = Date.now();
    if (now - lastRefetchRef.current < MARKETS_LIVE_REFRESH_MS) return;
    lastRefetchRef.current = now;
    void queryClient.invalidateQueries({ queryKey: ['/v1/markets'] });
  }, [streamLatest, queryClient]);
  const [filter, setFilter] = useState('');
  // The volume sort puts pure CEX reference pairs first: a visitor asking
  // "what trades on Stellar" got Binance's tape. Default view: Stellar.
  const [venue, setVenue] = useState<Venue>('stellar');

  const sorted = useMemo(() => {
    let rows = data?.markets ?? [];
    const isStellarPair = (m: { base?: string; quote?: string }) =>
      isStellarAsset(m.base ?? '') || isStellarAsset(m.quote ?? '');
    if (!assetParam && venue !== 'all') {
      rows = rows.filter((m) =>
        venue === 'stellar' ? isStellarPair(m) : !isStellarPair(m),
      );
    }
    const q = filter.trim().toLowerCase();
    if (!q) return rows;
    return rows.filter((m) =>
      `${m.base ?? ''} ${m.quote ?? ''}`.toLowerCase().includes(q),
    );
  }, [data, filter, venue, assetParam]);

  function setOrder(next: Order) {
    const sp = new URLSearchParams(params.toString());
    if (next === 'volume_24h_usd_desc') sp.delete('order');
    else sp.set('order', next);
    router.replace(`/markets${sp.toString() ? `?${sp.toString()}` : ''}`);
  }
  // Server-side sort expressed in the shared header's terms, so the markers
  // match every other table.
  const sort: SortState<SortKey> =
    orderBy === 'pair'
      ? { key: 'pair', dir: 'asc' }
      : { key: 'volume', dir: 'desc' };
  const onSort = (key: SortKey) =>
    setOrder(
      key === 'pair' && orderBy !== 'pair' ? 'pair' : 'volume_24h_usd_desc',
    );
  const ariaSort = (key: SortKey) =>
    sort.key !== key ? 'none' : sort.dir === 'asc' ? 'ascending' : 'descending';

  const source = asExample('/v1/markets', {
    limit: 100,
    order_by: orderBy,
    include: 'sparkline',
  });

  if (isError) {
    return (
      <Callout tone="bad" title="Markets failed to load">
        {error instanceof Error ? error.message : 'unknown error'}
      </Callout>
    );
  }

  const loading = isLoading || !data;
  const total = data?.markets.length ?? 0;
  const shareRows = sorted.map((m) => ({
    id: `${m.base}|${m.quote}`,
    label: `${shortAssetText(m.base)}/${shortAssetText(m.quote)}`,
    volume: m.volume_24h_usd,
  }));
  const hasShare = topWithOther(shareRows, 1).top.length > 0;

  return (
    <div className="space-y-4">
      <div className="flex flex-col gap-3 md:flex-row md:items-center md:justify-between">
        <div className="flex flex-wrap items-center gap-3">
          <div className="relative w-full sm:w-80">
            <Search
              aria-hidden="true"
              className="text-ink-faint pointer-events-none absolute top-1/2 left-3 h-4 w-4 -translate-y-1/2"
            />
            <Input
              type="search"
              aria-label={`Filter the ${total} markets shown by base or quote asset (does not search beyond the top ${total} by volume)`}
              // Filters the fetched top-N page only, never every active pair
              // on Stellar; the copy says so.
              placeholder={
                loading
                  ? 'Filter pairs by base or quote…'
                  : `Filter these ${total} pairs by base or quote…`
              }
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
              disabled={loading}
              className="pl-9"
            />
          </div>
          {!assetParam && (
            <Segmented
              ariaLabel="Venue"
              options={VENUE_OPTIONS}
              value={venue}
              onChange={(v) => setVenue(v as Venue)}
            />
          )}
        </div>
        <div className="text-ink-muted flex flex-wrap items-center gap-3 text-xs">
          {data && <FreshnessMarker flags={data.flags} />}
          {data && (
            <span className="tnum">
              {sorted.length === total
                ? `${total} pairs`
                : `${sorted.length} of ${total} pairs`}
              {filter && (
                <button
                  type="button"
                  onClick={() => setFilter('')}
                  className="text-brand-600 ml-2 hover:underline"
                >
                  clear
                </button>
              )}
            </span>
          )}
          <RequestReveal example={source} position="inline" />
        </div>
      </div>

      {assetParam && (
        <Callout tone="info">
          Showing markets for <code className="font-mono">{assetParam}</code>:
          pairs where it is the base or the quote.{' '}
          <Link href="/markets" className="font-medium hover:underline">
            Clear filter
          </Link>
        </Callout>
      )}

      {!loading && total === 0 ? (
        <EmptyState
          headingLevel={2}
          title="No active markets"
          description={
            assetParam
              ? `No pair involving ${assetParam} traded in the last 14 days.`
              : 'No pair traded in the last 14 days.'
          }
          action={
            assetParam ? (
              <ButtonLink href="/markets" variant="secondary" size="sm">
                View all markets
              </ButtonLink>
            ) : undefined
          }
        />
      ) : (
        <>
          {hasShare && (
            <Panel
              headingLevel={2}
              title="Volume share"
              hint="How the listed pairs split the trailing-24h USD volume"
              source={source}
            >
              <VolumeShare
                noun="pairs"
                rows={shareRows}
                scopeNote={`Only the top ${total} pairs by volume are fetched; thousands more traded in the last 14 days.`}
              />
            </Panel>
          )}
          <TableWrap>
            <Table>
              <THead>
                <tr>
                  <Th className={COL.sm}>#</Th>
                  <SortableTh
                    label="Base"
                    sortKey="pair"
                    sort={sort}
                    onSort={onSort}
                    ariaSort={ariaSort}
                  />
                  <Th>Quote</Th>
                  <Th align="right" className={COL.sm}>
                    Last price
                  </Th>
                  <SortableTh
                    label="24h volume"
                    sortKey="volume"
                    align="right"
                    sort={sort}
                    onSort={onSort}
                    ariaSort={ariaSort}
                  />
                  <Th align="right" className={COL.md}>
                    24h trades
                  </Th>
                  <Th className={COL.lg}>Hourly volume</Th>
                  <Th align="right" className={COL.md}>
                    Last trade
                  </Th>
                </tr>
              </THead>
              <TBody>
                {loading ? (
                  Array.from({ length: 12 }, (_, i) => <SkeletonRow key={i} />)
                ) : sorted.length === 0 ? (
                  <tr>
                    <td
                      colSpan={8}
                      className="text-ink-muted px-4 py-10 text-center text-sm"
                    >
                      No listed pair matches &ldquo;{filter}&rdquo;.
                    </td>
                  </tr>
                ) : (
                  sorted.map((m, i) => {
                    const href = `/markets/${encodeURIComponent(`${m.base}~${m.quote}`)}`;
                    return (
                      <TR key={`${m.base}|${m.quote}`}>
                        <Td className={cn('text-ink-faint tnum', COL.sm)}>
                          {i + 1}
                        </Td>
                        <Td>
                          <Link
                            href={href}
                            className="hover:text-brand-600 block"
                          >
                            <AssetLabel canonical={m.base} />
                          </Link>
                        </Td>
                        <Td>
                          <Link
                            href={href}
                            className="hover:text-brand-600 block"
                          >
                            <AssetLabel canonical={m.quote} />
                          </Link>
                        </Td>
                        <Td align="right" className={COL.sm}>
                          <LastPriceCell raw={m.last_price} />
                        </Td>
                        <Td align="right">
                          {m.volume_24h_usd ? (
                            <span className="text-ink font-mono">
                              ${formatCompactUnits(m.volume_24h_usd)}
                            </span>
                          ) : (
                            <span className="text-ink-faint">—</span>
                          )}
                        </Td>
                        <Td align="right" className={COL.md}>
                          <span className="font-mono">
                            {formatCompact(m.trade_count_24h)}
                          </span>
                        </Td>
                        <Td className={COL.lg}>
                          <SourceSparkline buckets={m.volume_history_24h} />
                        </Td>
                        <Td
                          align="right"
                          className={cn(
                            'text-ink-muted font-mono text-xs',
                            COL.md,
                          )}
                        >
                          {formatRelative(m.last_trade_at)}
                        </Td>
                      </TR>
                    );
                  })
                )}
              </TBody>
            </Table>
          </TableWrap>
        </>
      )}
    </div>
  );
}

function SkeletonRow() {
  const num = <Skeleton className="ml-auto h-3 w-16" />;
  return (
    <TR aria-hidden="true">
      <Td className={COL.sm}>
        <Skeleton className="h-3 w-4" />
      </Td>
      <Td>
        <Skeleton className="h-3 w-20" />
        <Skeleton className="mt-1.5 h-3 w-14" />
      </Td>
      <Td>
        <Skeleton className="h-3 w-16" />
      </Td>
      <Td align="right" className={COL.sm}>
        {num}
      </Td>
      <Td align="right">{num}</Td>
      <Td align="right" className={COL.md}>
        {num}
      </Td>
      <Td className={COL.lg}>
        <Skeleton className="h-6 w-20" />
      </Td>
      <Td align="right" className={COL.md}>
        {num}
      </Td>
    </TR>
  );
}
