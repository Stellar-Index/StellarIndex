import type { Metadata } from 'next';
import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';

import {
  buildFetchData,
  buildFetchPairPrice,
  requireRows,
} from '@/lib/buildFetch';
import {
  baseUnitsDecimal,
  formatPriceSmall,
  compareDecimalStrings,
  formatCompactUnits,
  formatPairPrice,
  ratioPct,
  sumDecimalStrings,
  formatUnitsReadable,
} from '@/lib/format';
import {
  serializeJsonLd,
  datasetJsonLd,
  ogImageFor,
  shellMetadata,
} from '@/lib/seo';
import {
  Badge,
  Container,
  EmptyState,
  PageHeader,
  TBody,
  Table,
  Td,
  Th,
  THead,
  LinkRow,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { Panel } from '@/components/reveal';
import { asExample } from '@/api/client';
import { EntityNotFoundShell } from '@/components/EntityNotFoundShell';
import { Suspense } from 'react';

import { LivePairPrice } from './LivePairPrice';
import { OrderBookPanel } from './OrderBookPanel';
import { PairChart } from './PairChart';
import { PairPathView } from './PairPathView';
import { SourceBreakdown } from './SourceBreakdown';
import { SourcePriceSpread } from './SourcePriceSpread';
import { TradeScatter } from './TradeScatter';
import { shortAssetText } from '@/lib/asset-label';
import { assetHref } from '@/lib/fiat-slugs';
import { AssetText } from '@/components/AssetLink';
import { CURRENT_NETWORK, USDC_ASSET_ID } from '@/lib/networks';
import type { TradeRow } from '@/api/hooks';

type Params = Promise<{ pair: string }>;

interface Market {
  base: string;
  quote: string;
  trade_count_24h: number;
  last_trade_at: string;
  volume_24h_usd?: string | null;
}

interface ChartPoint {
  t: string;
  p: string;
  v_usd?: string | null;
}

interface ChartResp {
  asset_id: string;
  quote: string;
  granularity?: string;
  timeframe?: string;
  price_type?: string;
  points: ChartPoint[];
}

interface OhlcResp {
  from: string;
  to: string;
  open: string;
  high: string;
  low: string;
  close: string;
  base_volume: string;
  quote_volume: string;
  // Decimal exponent of the two volume integers above. The smallest unit
  // is the per-SOURCE scale of the venues that traded in the window — 7
  // on-chain, 8 on a CEX, 6 on the FX feeds — so it must be read off the
  // bar, never assumed. Optional here only so the page degrades honestly
  // against an API older than this field rather than rendering a figure
  // scaled by a guess.
  base_volume_decimals?: number;
  quote_volume_decimals?: number;
  trade_count: number;
  truncated: boolean;
}

const PAIR_SEPARATOR = '~';

/**
 * Pair slug = `${base}${PAIR_SEPARATOR}${quote}`, URL-encoded once.
 * `~` is a URL-safe char that doesn't appear in any Stellar
 * canonical asset identifier (asset_ids use `-`, `:`, alphanum;
 * the off-chain `crypto:BTC` / `fiat:USD` shapes likewise don't
 * contain `~`). Single decodeURIComponent restores the asset_ids
 * verbatim — the backend accepts them as-is on the `asset=` and
 * `quote=` query parameters.
 */
function pairSlug(base: string, quote: string): string {
  return `${base}${PAIR_SEPARATOR}${quote}`;
}

function decodePairSlug(slug: string): { base: string; quote: string } | null {
  const decoded = decodeURIComponent(slug);
  const ix = decoded.indexOf(PAIR_SEPARATOR);
  if (ix === -1) return null;
  return { base: decoded.slice(0, ix), quote: decoded.slice(ix + 1) };
}

export async function generateStaticParams() {
  // Top 500 pairs by 24h volume — 100 was too few: /markets/native~AQUA-G… (a natural
  // click-through from /assets/AQUA) 404'd because AQUA pairs
  // didn't crack the top 100 by USD volume. AQUA is the
  // 4th-largest asset on Stellar by trade count but its dominant
  // pair is XLM-quoted, which underweights it on the
  // USD-volume sort. 500 covers the long tail visible in the
  // /markets listing pagination.
  //
  // CF Pages build cost: ~50KB × 500 = ~25MB of HTML, well within
  // bounds. Build time ≈ 4-5 min for the entire markets section,
  // parallelised across page workers.
  const fallback = [
    {
      pair: pairSlug('native', USDC_ASSET_ID),
    },
  ];
  // Fail-hard (src/lib/buildFetch.ts): an unreachable or empty markets
  // listing throws so the build fails instead of exporting only the
  // fallback route. CI-stub builds fall through to the fallback.
  const markets = requireRows(
    await buildFetchData<Market[]>(
      '/v1/markets?limit=500&order_by=volume_24h_usd_desc',
    ),
    '/v1/markets listing for /markets/[pair] static params',
  );
  const out = markets
    .filter((m) => m.base && m.quote)
    .map((m) => ({ pair: pairSlug(m.base, m.quote) }));
  // 'shell' is the runtime-fallback sentinel: functions/markets/[[path]].js
  // serves its HTML for any pair outside this build-time snapshot. Without
  // it, markets that entered the ranking since the last deploy hard-404'd
  // — including pairs the /markets listing itself was linking to (row 1 of
  // its own table) and 2 of the 8 rows in /network's "Top Stellar markets"
  // widget. Same pattern as accounts/contracts/issuers/ledgers/transactions.
  //
  // Note this is NOT a cutoff problem: the 404ing pairs ranked 27, 51 and
  // 100 in live data, well inside the 500 limit. The snapshot was stale,
  // not small, which is why raising the limit from 100 to 500
  // did not hold.
  return [{ pair: 'shell' }, ...(out.length > 0 ? out : fallback)];
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { pair } = await params;
  // The runtime-fallback shell, and any slug without a `~` separator, is
  // one baked document standing in for arbitrary /markets/* URLs.
  const decoded = pair === 'shell' ? null : decodePairSlug(pair);
  if (!decoded) {
    return shellMetadata(
      'Pair',
      'Stellar market pair detail, rendered live from the Stellar Index API.',
    );
  }
  const baseLabel = shortAssetText(decoded.base);
  const quoteLabel = shortAssetText(decoded.quote);
  // Best-effort price fetch so the social-share preview reads as
  // a real ticker rather than boilerplate.
  const price = await buildFetchPairPrice(decoded.base, decoded.quote);
  const priceStr = price?.price ? formatPriceSmall(price.price) : '—';
  const suffix = priceStr === '—' ? '' : ` ${priceStr}`;
  const title = `${baseLabel} / ${quoteLabel}${suffix} — pair detail`;
  const description = `Live VWAP${suffix ? ` (${suffix.trim()})` : ''}, recent trades, and per-source breakdown for ${baseLabel} / ${quoteLabel} on Stellar.`;
  // Canonical URL: the URL-encoded pair slug. Without this,
  // any case- or encoding-variant of the same pair would be
  // treated as a separate page by Google.
  // The route param arrives ALREADY URL-encoded from generateStaticParams;
  // encoding again would yield %253A canonicals that 404. Decode-then-encode
  // is idempotent for both encoded and raw inputs; trailing slash matches
  // the site's canonical form (trailingSlash: true).
  const canonical = `${CURRENT_NETWORK.explorerUrl}/markets/${encodeURIComponent(decodeURIComponent(pair))}/`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: [ogImageFor('markets', pair)],
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: [ogImageFor('markets', pair)],
    },
  };
}

// Enrichment fetches below ride src/lib/buildFetch.ts: retried +
// memoised, THROW on persistent transport failure (fail-hard), and
// return null/[] only when the API authoritatively answered 4xx —
// a pair with no direct VWAP or no recent trades is a legitimate
// state the page renders honestly as "—".
//
// EXCEPTION: fetchPrice passes softFail — the live price is refreshed
// client-side by <LivePrice>, so a cold/slow/unreachable /v1/price must
// NOT abort the export (the edge has been seen hanging ~25s on a
// cold-cache asset, landing on a different one each run). It degrades to
// no build-time price instead of throwing; short timeout + few attempts
// keep a hung endpoint from stalling the build.
function fetchChart(base: string, quote: string): Promise<ChartResp | null> {
  return buildFetchData<ChartResp>(
    `/v1/chart?asset=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&timeframe=24h&granularity=1h`,
  );
}

function fetchOhlc(base: string, quote: string): Promise<OhlcResp | null> {
  // /v1/ohlc returns a single bar over [from, to). Default window
  // is "now - 1h" → "now" when both are absent — exactly what we
  // want for the markets-pair OHLC strip. The earlier `interval=1h`
  // param was silently ignored (not in the API schema) but the
  // default-window happened to match. Drop it for honesty.
  // softFail like fetchPrice: the trailing-hour bar is ephemeral, and a
  // typed 422 (every trade in the hour filtered as an outlier) is a real
  // state that must render no strip, not abort the export.
  return buildFetchData<OhlcResp>(
    `/v1/ohlc?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}`,
    { softFail: true, timeoutMs: 6_000, attempts: 2 },
  );
}

async function fetchHistory(base: string, quote: string): Promise<TradeRow[]> {
  const rows = await buildFetchData<TradeRow[]>(
    `/v1/history?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&limit=50`,
  );
  return rows ?? [];
}

interface PoolRow {
  source: string;
  trade_count_24h: number;
  volume_24h_usd?: string | null;
  last_price?: string | null;
  last_trade_at: string;
}

async function fetchSourceBreakdown(
  base: string,
  quote: string,
): Promise<PoolRow[]> {
  // /v1/pools?base=&quote= returns one row per source contributing
  // to this exact pair. Naturally sorted by 24h USD volume (the
  // endpoint default), which is the right order for the panel.
  const rows = await buildFetchData<PoolRow[]>(
    `/v1/pools?base=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&limit=50`,
  );
  return rows ?? [];
}

export default async function PairPage({ params }: { params: Params }) {
  const { pair } = await params;
  // Runtime-fallback shell for pairs outside the build-time pre-render.
  // functions/markets/[[path]].js serves this HTML for any unmatched
  // /markets/* path, and the client view reads the pair from the URL
  // (the pre-render is a stale snapshot, not a small one).
  if (pair === 'shell') {
    return (
      <Suspense fallback={null}>
        <PairPathView />
      </Suspense>
    );
  }
  const decoded = decodePairSlug(pair);
  if (!decoded) {
    return <PairNotFound />;
  }
  const { base, quote } = decoded;

  const [price, chart, ohlc, history, sourceBreakdown] = await Promise.all([
    buildFetchPairPrice(base, quote),
    fetchChart(base, quote),
    fetchOhlc(base, quote),
    fetchHistory(base, quote),
    fetchSourceBreakdown(base, quote),
  ]);

  const baseLabel = shortAssetText(base);
  const quoteLabel = shortAssetText(quote);

  // Per-source breakdown: count trades by source in the history sample.
  // (The full 24h source distribution is rendered by SourceBreakdownPanel
  // below, which pulls real volume from /v1/pools?base=&quote=. perSource
  // here remains for the Activity panel's "Sources: N" stat.)
  const perSource = new Map<string, number>();
  for (const t of history) {
    perSource.set(t.source, (perSource.get(t.source) ?? 0) + 1);
  }

  // 24h change: the server's own computed figure — never
  // re-derived from two /v1/chart points, which disagrees with it
  // whenever a bucket is missing, backfilled, or the window boundary
  // doesn't land exactly on the two samples picked here.
  const points = chart?.points ?? [];
  const change24hPct = price?.change_24h_pct;
  const change24hNum = change24hPct != null ? Number(change24hPct) : null;
  const change24h =
    change24hNum != null && Number.isFinite(change24hNum) ? change24hNum : null;
  const volume24hUsd = formatUsd(
    sumDecimalStrings(points.map((pt) => pt.v_usd)),
  );

  // BreadcrumbList JSON-LD derives from the visible Crumb[] inside Breadcrumbs below.
  // schema.org Dataset — eligibility for Google Dataset Search. contentUrl
  // points at the real public /v1/chart endpoint this page already uses.
  const datasetLD = datasetJsonLd({
    name: `${baseLabel}/${quoteLabel} price & volume — Stellar Index`,
    description: `Aggregated volume-weighted average price (VWAP), OHLC candles, and trade volume for the ${baseLabel}/${quoteLabel} market on Stellar, computed by Stellar Index across on-chain venues (SDEX, AMMs) and tracked exchanges.`,
    url: `${CURRENT_NETWORK.explorerUrl}/markets/${encodeURIComponent(`${base}~${quote}`)}`,
    keywords: [
      baseLabel,
      quoteLabel,
      `${baseLabel} price`,
      `${baseLabel} ${quoteLabel}`,
      'Stellar',
      'VWAP',
      'OHLC',
    ],
    variableMeasured: ['VWAP', 'OHLC', 'trade volume', '24h price change'],
    contentUrl: `${CURRENT_NETWORK.apiBaseUrl}/v1/chart?asset=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}&timeframe=24h&granularity=1h`,
  });
  return (
    <Container className="space-y-6 py-8">
      <script
        type="application/ld+json"
        dangerouslySetInnerHTML={{ __html: serializeJsonLd(datasetLD) }}
      />
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Markets', href: '/markets' },
          { label: `${baseLabel} / ${quoteLabel}` },
        ]}
        title={
          <>
            <AssetBadge canonical={base} /> / <AssetBadge canonical={quote} />
          </>
        }
        meta={`${quoteLabel} per ${baseLabel}, aggregated across every venue indexed`}
        actions={
          price?.price_type && (
            <Badge tone="neutral">
              <span className="font-mono">{price.price_type}</span>
            </Badge>
          )
        }
      />

      <section className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        <Panel
          headingLevel={2}
          title="Price"
          hint="Live, volume-weighted across every venue indexed"
          source={asExample('/v1/price', { asset: base, quote })}
          className="lg:col-span-2"
        >
          <div className="flex flex-wrap items-baseline gap-4">
            <LivePairPrice
              base={base}
              quote={quote}
              initialPrice={price?.price ?? null}
              initialObservedAt={price?.observed_at ?? null}
              quoteIsUsd={isUsdQuote(quote)}
              quoteSuffix={shortAssetText(quote)}
              initialChangePct={change24h}
            />
          </div>
          <div className="mt-4">
            <PairChart
              base={base}
              quote={quote}
              baseLabel={baseLabel}
              quoteLabel={quoteLabel}
            />
          </div>
        </Panel>

        <SourceBreakdown headingLevel={2} base={base} quote={quote} />

        <Panel
          headingLevel={2}
          title="Recent activity"
          hint={`last ${history.length} trades`}
          source={asExample('/v1/history', { base, quote, limit: 50 })}
        >
          <dl className="grid grid-cols-2 gap-4">
            <Stat label="Trades in sample" value={String(history.length)} />
            <Stat label="Sources in sample" value={String(perSource.size)} />
            {points.length > 0 && (
              <Stat
                label="Last hour USD vol"
                value={formatUsd(points[points.length - 1].v_usd)}
              />
            )}
            {volume24hUsd !== '—' && (
              <Stat label="24h USD vol" value={volume24hUsd} />
            )}
          </dl>
        </Panel>

        {ohlc && (
          <Panel
            headingLevel={2}
            title="OHLC, last hour"
            hint={`${ohlc.from.slice(0, 16).replace('T', ' ')} to ${ohlc.to.slice(11, 16)} UTC`}
            source={asExample('/v1/ohlc', { base, quote })}
            className="lg:col-span-2"
          >
            <dl className="grid grid-cols-2 gap-4 sm:grid-cols-3">
              <Stat label="Open" value={ohlc.open} />
              <Stat label="High" value={ohlc.high} />
              <Stat label="Low" value={ohlc.low} />
              <Stat label="Close" value={ohlc.close} />
              <Stat
                label={`Quote vol (${shortAssetText(quote)})`}
                value={formatScaledAmount(
                  ohlc.quote_volume,
                  ohlc.quote_volume_decimals,
                  quote,
                )}
              />
              <Stat
                label="Trades"
                value={ohlc.trade_count.toLocaleString('en-US')}
              />
            </dl>
          </Panel>
        )}
      </section>

      {sourceBreakdown.length > 0 && (
        <SourceBreakdownPanel
          rows={sourceBreakdown}
          base={base}
          quote={quote}
        />
      )}

      <OrderBookPanel base={base} quote={quote} />

      <Panel
        headingLevel={2}
        title="Recent trades"
        hint={
          history.length > 0
            ? `${history.length} most recent across all sources`
            : undefined
        }
        source={asExample('/v1/history', { base, quote, limit: 50 })}
      >
        {history.length > 0 ? (
          <>
            <TradeScatter trades={history} />
            <div className="border-line -mx-4 mt-4 -mb-4 max-h-[32rem] overflow-auto border-t">
              <Table>
                <THead className="sticky top-0 z-10">
                  <tr>
                    <Th>Time</Th>
                    <Th>Source</Th>
                    <Th align="right">Price</Th>
                    <Th align="right">Base amount</Th>
                    <Th align="right">Quote amount</Th>
                  </tr>
                </THead>
                <TBody>
                  {history.map((t, i) => (
                    <LinkRow
                      href={`/transactions/${t.tx_hash}/`}
                      key={`${t.tx_hash ?? ''}|${t.op_index ?? i}|${t.ts}`}
                    >
                      <Td className="text-ink-muted font-mono text-xs whitespace-nowrap">
                        {t.tx_hash ? (
                          <Link
                            href={`/transactions/${t.tx_hash}/`}
                            className="hover:text-brand-600"
                          >
                            <TradeTime iso={t.ts} />
                          </Link>
                        ) : (
                          <TradeTime iso={t.ts} />
                        )}
                      </Td>
                      <Td>
                        <Link
                          href={hrefFor.source(t.source)}
                          className="hover:text-brand-600 font-mono text-xs"
                        >
                          {t.source}
                        </Link>
                      </Td>
                      <Td align="right" className="font-mono">
                        {t.price ?? '—'}
                      </Td>
                      <Td align="right" className="font-mono">
                        {formatUnitsReadable(
                          t.base_amount,
                          t.base_decimals ?? 7,
                        )}
                      </Td>
                      <Td align="right" className="font-mono">
                        {formatUnitsReadable(
                          t.quote_amount,
                          t.quote_decimals ?? 7,
                        )}
                      </Td>
                    </LinkRow>
                  ))}
                </TBody>
              </Table>
            </div>
          </>
        ) : (
          <EmptyState
            title="No recent trades"
            description="No trades returned for this pair in the last sample."
          />
        )}
      </Panel>
    </Container>
  );
}

function SourceBreakdownPanel({
  rows,
  base,
  quote,
}: {
  rows: PoolRow[];
  base: string;
  quote: string;
}) {
  // `volume_24h_usd` is null on sources that traded but the aggregator has
  // not priced; those rows show "—" and stay out of the share denominator.
  const totalUSD = sumDecimalStrings(rows.map((r) => r.volume_24h_usd));
  return (
    <Panel
      headingLevel={2}
      title="Sources contributing"
      hint={`${rows.length} venue${rows.length === 1 ? '' : 's'}, ranked by 24h USD volume`}
      source={asExample('/v1/pools', { base, quote, limit: 50 })}
    >
      <SourcePriceSpread rows={rows} />
      <div className="border-line -mx-4 mt-4 -mb-4 overflow-x-auto border-t">
        <Table>
          <THead>
            <tr>
              <Th>Source</Th>
              <Th align="right">Last price</Th>
              <Th align="right">24h volume</Th>
              <Th align="right" className="w-48">
                Share
              </Th>
            </tr>
          </THead>
          <TBody>
            {rows.map((r) => {
              const pct = ratioPct(r.volume_24h_usd, totalUSD, 1);
              const lpFixed = r.last_price
                ? formatPairPrice(r.last_price)
                : null;
              const priced =
                r.volume_24h_usd != null &&
                compareDecimalStrings(r.volume_24h_usd, '0') === 1;
              return (
                <LinkRow href={hrefFor.source(r.source)} key={r.source}>
                  <Td>
                    <Link
                      href={hrefFor.source(r.source)}
                      className="hover:text-brand-600 font-mono text-xs"
                    >
                      {r.source}
                    </Link>
                  </Td>
                  <Td align="right" className="text-ink-muted font-mono">
                    {lpFixed ?? '—'}
                  </Td>
                  <Td align="right" className="font-mono">
                    {priced ? `$${formatCompactUnits(r.volume_24h_usd!)}` : '—'}
                  </Td>
                  <Td align="right">
                    <span className="inline-flex w-full items-center justify-end gap-3">
                      <span
                        aria-hidden="true"
                        className="bg-surface-subtle h-1.5 w-24 overflow-hidden rounded-sm"
                      >
                        <span
                          className="bg-brand-500 block h-full"
                          style={{ width: `${pct ?? 0}%` }}
                        />
                      </span>
                      <span className="text-ink-muted w-10 font-mono text-xs">
                        {pct != null ? `${pct.toFixed(0)}%` : '—'}
                      </span>
                    </span>
                  </Td>
                </LinkRow>
              );
            })}
          </TBody>
        </Table>
      </div>
    </Panel>
  );
}

function PairNotFound() {
  return (
    <EntityNotFoundShell
      title="Pair not found"
      description={
        <>
          The slug must be in the form{' '}
          <code className="font-mono">{`base${PAIR_SEPARATOR}quote`}</code>.
        </>
      }
      backHref="/markets"
      backLabel="Browse all markets"
    />
  );
}

// Phones get the clock; the full stamp returns from sm and stays in the title.
function TradeTime({ iso }: { iso: string }) {
  const full = formatTimestamp(iso);
  return (
    <span title={full}>
      <span className="sm:hidden">{full.slice(11, 19)}</span>
      <span className="hidden sm:inline">{full}</span>
    </span>
  );
}

function Stat({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className={PANEL_LABEL_CLASS}>{label}</dt>
      <dd className="text-ink mt-1 font-mono text-sm tabular-nums">{value}</dd>
    </div>
  );
}

// AssetBadge renders an asset's short ticker as a link to its asset
// page, so a pair's two legs are click-throughs (e.g. XLM / USDC each
// open /assets/{id}).
function AssetBadge({ canonical }: { canonical: string }) {
  let label = canonical;
  // slug is the static-export-safe link target — the SHORT code/ticker
  // form (`/assets/USDC`), never the long asset_id (`/assets/USDC-GA5Z…`
  // 404s: long-form ids aren't pre-rendered). null → render plain text.
  let slug: string | null = null;
  if (canonical === 'native' || /^\d+$/.test(canonical)) {
    label = 'XLM';
    slug = 'native';
  } else if (canonical.startsWith('fiat:') || canonical.startsWith('crypto:')) {
    // A bare ticker has a page only if the catalogue lists it (fiat and
    // most crypto live under /external/assets); AssetText checks.
    return <AssetText canonical={canonical} />;
  } else if (/^C[A-Za-z0-9]{55}$/.test(canonical)) {
    // Raw SAC contract — no safe asset route; show truncated, no link.
    label = `${canonical.slice(0, 4)}…${canonical.slice(-4)}`;
  } else {
    const dashIx = canonical.indexOf('-');
    if (dashIx !== -1) {
      // Link the FULL canonical id, not the bare code — the
      // code form 404s for anything outside the top-500 listing AND
      // can resolve to the wrong issuer's asset on code collisions
      // (every USDC-alike shares /assets/USDC). The canonical-id
      // routes are emitted for exactly the same set of assets, so
      // this never links worse and always links precisely.
      label = canonical.slice(0, dashIx);
      slug = canonical;
    } else if (canonical.length <= 12) {
      slug = canonical;
    }
  }
  if (!slug) return <span>{label}</span>;
  return (
    <Link
      href={assetHref(slug)}
      className="hover:text-brand-600 transition-colors"
    >
      {label}
    </Link>
  );
}

// Prices on this page are quote-per-base — a "$"
// prefix is only honest when the quote is USD or USD-pegged. Other
// quotes (native, AQUA, EURC…) get the quote code as a suffix.
function isUsdQuote(quote: string): boolean {
  return (
    quote === 'fiat:USD' || /^USDC[:-]/.test(quote) || /^USDT[:-]/.test(quote)
  );
}

// Renders a raw smallest-unit volume integer in the quote asset's own
// units, using the scale the API stated for it (F096). A bar that states
// no scale renders as "—": the divisor is a property of the venues that
// traded, so guessing one is how this panel came to overstate every
// CEX-quoted pair tenfold. An em-dash is the honest answer. The scaled
// value stays an exact decimal string (ADR-0003) and is rounded from it.
function formatScaledAmount(
  raw: string,
  decimals: number | null | undefined,
  quote: string,
): string {
  if (decimals == null || !Number.isFinite(decimals) || decimals < 0) {
    return '—';
  }
  const exact = baseUnitsDecimal(raw, decimals);
  if (exact == null || compareDecimalStrings(exact, '0') !== 1) return '—';
  const num = formatPriceSmall(exact);
  return isUsdQuote(quote) ? `$${num}` : `${num} ${shortAssetText(quote)}`;
}

// Exact from the decimal string; a negative, zero or non-decimal value shows '—'.
function formatUsd(raw: string | null | undefined): string {
  if (raw == null || raw.trim().startsWith('-')) return '—';
  const s = formatCompactUnits(raw);
  if (s === '—' || /^0*\.?0*$/.test(raw.trim())) return '—';
  return s === '0' ? '<$0.01' : `$${s}`;
}

function formatTimestamp(iso: string): string {
  if (!iso) return '—';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toISOString().replace('T', ' ').slice(0, 19) + ' UTC';
}
