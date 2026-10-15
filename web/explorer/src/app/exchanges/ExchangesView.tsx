'use client';

import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { useQuery } from '@tanstack/react-query';

import { Panel } from '@/components/reveal';
import { apiGet, asExample } from '@/api/client';
import {
  compareDecimalDesc,
  compareDecimalStrings,
  formatCompact,
  formatCompactUnits,
  formatPairPrice,
  ratioPct,
  sumDecimalStrings,
} from '@/lib/format';
import { VolumeShare } from '@/components/charts/VolumeShare';
import { shortAssetText } from '@/lib/asset-label';
import {
  Container,
  PageHeader,
  Skeleton,
  Stat,
  StatCell,
  StatGrid,
  TBody,
  LinkRow,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';

// /v1/sources + /v1/markets rows from the generated OpenAPI contract,
// via the shared aliases in src/api/hooks.ts.
import type { Market, Source as SourceRow } from '@/api/hooks';

const LABEL: Record<string, string> = {
  binance: 'Binance',
  coinbase: 'Coinbase',
  kraken: 'Kraken',
  bitstamp: 'Bitstamp',
  poloniex_via_btc: 'Poloniex (via BTC)',
};

export function ExchangesView() {
  const q = useQuery<SourceRow[]>({
    queryKey: ['/v1/sources', 'stats,sparkline', 'cex'],
    queryFn: async () => {
      const env = await apiGet<{ data: SourceRow[] }>('/v1/sources', {
        include: 'stats,sparkline',
      });
      const arr = env.data ?? [];
      return arr
        .filter((s) => s.class === 'exchange' && s.subclass === 'cex')
        .sort((a, b) => compareDecimalDesc(a.volume_24h_usd, b.volume_24h_usd));
    },
  });

  // Absent (registry fetch failed) ≠ empty (no CEX registered): keep the
  // undefined so the table renders "unavailable" instead of the claim
  // "No CEX sources reporting."
  const rows = q.data ?? [];
  const registryAvailable = q.data != null;
  // `?include=stats` soft-fails server-side (internal/api/v1/sources.go:
  // "serve the registry without stats") and every stats column is
  // omitempty — on that degrade EVERY row arrives bare. Detect the
  // wholesale absence so the page renders '—', not a fabricated
  // "$0 across 0 trades" claim. When any row carries stats, a bare field
  // on another row is a genuine present-and-zero (omitempty drops 0).
  const statsAvailable = rows.some(
    (r) =>
      r.volume_24h_usd != null ||
      r.trade_count_24h != null ||
      r.markets_count_24h != null,
  );
  const totalVol = sumDecimalStrings(rows.map((r) => r.volume_24h_usd)) ?? '0';
  const totalTrades = rows.reduce((s, r) => s + (r.trade_count_24h ?? 0), 0);
  const totalMarkets = rows.reduce((s, r) => s + (r.markets_count_24h ?? 0), 0);

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Exchanges' }]}
        title="Exchanges"
        meta="Centralised venues feeding the reference price. Volume and trades cover the last 24 hours; pairs are what each venue reported in the last 14 days."
      />
      {rows.length > 0 && (
        <StatGrid cols={3}>
          <StatCell>
            <Stat
              label="24h volume"
              value={statsAvailable ? `$${formatCompactUnits(totalVol)}` : '—'}
            />
          </StatCell>
          <StatCell>
            <Stat
              label="24h trades"
              value={statsAvailable ? formatCompact(totalTrades) : '—'}
            />
          </StatCell>
          <StatCell>
            <Stat
              label="Pairs covered"
              value={
                statsAvailable ? totalMarkets.toLocaleString('en-US') : '—'
              }
            />
          </StatCell>
        </StatGrid>
      )}
      <Panel
        headingLevel={2}
        title={
          registryAvailable
            ? `${rows.length} centralised exchanges`
            : 'Centralised exchanges'
        }
        hint={
          statsAvailable
            ? `Total 24h: $${formatCompactUnits(totalVol)} across ${formatCompact(totalTrades)} trades on ${totalMarkets} pairs`
            : registryAvailable
              ? '24h stats unavailable — refreshing'
              : 'Venue registry'
        }
        source={asExample('/v1/sources', { include: 'stats' })}
        bodyClassName="-mx-4 -mb-4"
      >
        {statsAvailable && rows.length > 1 && (
          <div className="mx-4 mb-4">
            <VolumeShare
              noun="exchanges"
              topN={8}
              rows={rows.map((r) => ({
                id: r.name,
                label: LABEL[r.name] ?? r.name,
                volume: r.volume_24h_usd,
              }))}
              scopeNote="Centralised venues only; on-chain DEXes are at /dexes."
            />
          </div>
        )}
        <div className="overflow-x-auto">
          <Table>
            <THead>
              <tr>
                <Th className="w-10">#</Th>
                <Th>Exchange</Th>
                <Th align="right">24h volume</Th>
                <Th align="right">24h trades</Th>
                <Th align="right">Pairs</Th>
                <Th align="right">Share</Th>
              </tr>
            </THead>
            <TBody>
              {q.isLoading && (
                <TR>
                  <Td colSpan={6}>
                    <Skeleton className="h-5 w-48" />
                  </Td>
                </TR>
              )}
              {!q.isLoading && !registryAvailable && (
                <TR>
                  <Td colSpan={6} className="text-ink-muted">
                    Exchange registry unavailable right now.{' '}
                    <Link
                      href="/status"
                      className="text-brand-600 hover:underline"
                    >
                      Status
                    </Link>
                  </Td>
                </TR>
              )}
              {!q.isLoading && registryAvailable && rows.length === 0 && (
                <TR>
                  <Td colSpan={6} className="text-ink-muted">
                    No CEX sources reporting.
                  </Td>
                </TR>
              )}
              {rows.map((r, i) => {
                const share = ratioPct(r.volume_24h_usd, totalVol) ?? 0;
                const vol = r.volume_24h_usd;
                return (
                  <LinkRow href={hrefFor.exchange(r.name)} key={r.name}>
                    <Td className="text-ink-faint font-mono tabular-nums">
                      {i + 1}
                    </Td>
                    <Td>
                      <Link
                        href={hrefFor.exchange(r.name)}
                        className="text-ink hover:text-brand-600 font-medium"
                      >
                        {LABEL[r.name] ?? r.name}
                      </Link>
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {vol && compareDecimalStrings(vol, '0') === 1 ? (
                        `$${formatCompactUnits(vol)}`
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {r.trade_count_24h && r.trade_count_24h > 0
                        ? formatCompact(r.trade_count_24h)
                        : statsAvailable
                          ? '0'
                          : '—'}
                    </Td>
                    <Td align="right" className="font-mono tabular-nums">
                      {statsAvailable ? (r.markets_count_24h ?? 0) : '—'}
                    </Td>
                    <Td align="right">
                      {statsAvailable ? (
                        <span className="inline-flex items-center justify-end gap-2">
                          <span
                            aria-hidden
                            className="bg-line block h-1.5 w-16 overflow-hidden rounded-full"
                          >
                            <span
                              className="bg-brand-500 block h-full"
                              style={{ width: `${Math.min(100, share)}%` }}
                            />
                          </span>
                          <span className="font-mono tabular-nums">
                            {share.toFixed(1)}%
                          </span>
                        </span>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </Td>
                  </LinkRow>
                );
              })}
            </TBody>
          </Table>
        </div>
      </Panel>
      <AllCEXMarkets
        venues={registryAvailable ? rows.map((r) => r.name) : undefined}
        registryLoading={q.isLoading}
      />
    </Container>
  );
}

// /v1/markets row (MarketRow via the hooks Market alias) plus the
// client-side `source` tag AllCEXMarkets stamps on each row when it
// merges the per-venue fetches.
type CEXMarket = Market & {
  // Client-side: which venue the row was fetched for (the /v1/markets
  // wire row itself carries no source column).
  source?: string;
};

// AllCEXMarkets surfaces every CEX pair we observed in the last
// 14 days, sorted by 24h USD volume. The venue-scoped fetches run
// concurrently and merge client-side — no new API endpoint required,
// and matches the volume-sort across venues. `venues` is the registry
// table's CEX list, so a newly registered venue appears in both tables.
function AllCEXMarkets({
  venues,
  registryLoading,
}: {
  // undefined = registry fetch failed; the pair list is then unknowable.
  venues: string[] | undefined;
  registryLoading: boolean;
}) {
  const queries = useQuery<CEXMarket[]>({
    queryKey: ['/v1/markets', venues, 'all-cex'],
    enabled: venues != null,
    queryFn: async () => {
      const all = await Promise.all(
        (venues ?? []).map(async (v) => {
          const env = await apiGet<{ data: CEXMarket[] }>('/v1/markets', {
            source: v,
            limit: 200,
            order_by: 'volume_24h_usd_desc',
          });
          return (env.data ?? []).map((m) => ({ ...m, source: v }));
        }),
      );
      const merged = all.flat();
      return merged.sort((a, b) =>
        compareDecimalDesc(a.volume_24h_usd, b.volume_24h_usd),
      );
    },
  });

  // The queryFn is a Promise.all over the venue-scoped /v1/markets
  // calls — ONE 503 rejects the whole query. `queries.data` is then
  // undefined, and `?? []` would headline "0 CEX pairs" plus "No CEX
  // pairs reporting.". Keep the absence.
  const markets = queries.data ?? [];
  const marketsAvailable = queries.data != null;
  const loading = registryLoading || queries.isLoading;

  return (
    <Panel
      headingLevel={2}
      title={
        marketsAvailable
          ? `${markets.length} CEX pairs · sorted by 24h volume`
          : 'CEX pairs · sorted by 24h volume'
      }
      hint="One row per (venue, base, quote) tuple — every pair observed across all registered CEXes in the last 14 days"
      source={asExample('/v1/markets', { source: 'binance', limit: 200 })}
      bodyClassName="-mx-4 -mb-4"
    >
      <div className="overflow-x-auto">
        <Table>
          <THead>
            <tr>
              <Th className="w-10">#</Th>
              <Th>Venue</Th>
              <Th>Pair</Th>
              <Th align="right">Last price</Th>
              <Th align="right">24h volume</Th>
              <Th align="right">24h trades</Th>
            </tr>
          </THead>
          <TBody>
            {loading && (
              <TR>
                <Td colSpan={6}>
                  <Skeleton className="h-5 w-64" />
                </Td>
              </TR>
            )}
            {!loading && !marketsAvailable && (
              <TR>
                <Td colSpan={6} className="text-ink-muted">
                  Pair list unavailable right now. The venue registry or one of
                  the venue fetches failed.{' '}
                  <Link
                    href="/status"
                    className="text-brand-600 hover:underline"
                  >
                    Status
                  </Link>
                </Td>
              </TR>
            )}
            {!loading && marketsAvailable && markets.length === 0 && (
              <TR>
                <Td colSpan={6} className="text-ink-muted">
                  No CEX pairs reporting.
                </Td>
              </TR>
            )}
            {markets.map((m, i) => {
              const slug = `${m.base}~${m.quote}`;
              const vol = m.volume_24h_usd;
              return (
                <LinkRow
                  href={`/markets/${encodeURIComponent(slug)}`}
                  key={`${m.source}:${slug}`}
                >
                  <Td className="text-ink-faint font-mono tabular-nums">
                    {i + 1}
                  </Td>
                  <Td>
                    {m.source ? (
                      <Link
                        href={hrefFor.exchange(m.source)}
                        className="text-ink-muted hover:text-brand-600"
                      >
                        {LABEL[m.source] ?? m.source}
                      </Link>
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td>
                    <Link
                      href={`/markets/${encodeURIComponent(slug)}`}
                      className="text-ink hover:text-brand-600 font-medium"
                    >
                      {shortAssetText(m.base)}
                      <span className="text-ink-faint">/</span>
                      {shortAssetText(m.quote)}
                    </Link>
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {m.last_price &&
                    compareDecimalStrings(m.last_price, '0') != null ? (
                      formatPairPrice(m.last_price)
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {vol && compareDecimalStrings(vol, '0') === 1 ? (
                      `$${formatCompactUnits(vol)}`
                    ) : (
                      <span className="text-ink-faint">—</span>
                    )}
                  </Td>
                  <Td align="right" className="font-mono tabular-nums">
                    {m.trade_count_24h && m.trade_count_24h > 0
                      ? formatCompact(m.trade_count_24h)
                      : '—'}
                  </Td>
                </LinkRow>
              );
            })}
          </TBody>
        </Table>
      </div>
      <p className="text-ink-faint border-line border-t px-4 py-3 text-xs">
        Curated pairs per venue · open a venue for its full list
      </p>
    </Panel>
  );
}
