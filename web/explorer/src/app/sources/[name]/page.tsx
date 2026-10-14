import type { Metadata } from 'next';
import Link from 'next/link';
import { Suspense } from 'react';

import { EntityNotFoundShell } from '@/components/EntityNotFoundShell';
import { Panel } from '@/components/reveal';
import {
  Badge,
  Container,
  PageHeader,
  TBody,
  TR,
  Table,
  Td,
  Th,
  THead,
} from '@/components/ui';
import { PANEL_LABEL_CLASS } from '@/components/ui/Page';
import { sourceLabel, sourceUnitsLabel } from '../sourceLabel';
import { SourceHealthPanel } from './SourceHealthPanel';
import { SourcePathView } from './SourcePathView';
import { SourcePeerBars } from './SourcePeerBars';
import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { SourceTopChart } from '@/app/dexes/[source]/SourceTopChart';
import { buildFetchData, failBuild, requireRows } from '@/lib/buildFetch';
import {
  formatCompact,
  formatCompactUnits,
  formatPairPrice,
} from '@/lib/format';
import { asExample } from '@/api/client';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES, shellMetadata } from '@/lib/seo';
import { shortAssetText } from '@/lib/asset-label';
import { cn } from '@/lib/cn';
import { CURRENT_NETWORK } from '@/lib/networks';

// Sources that also have a dedicated DEX or CEX detail page — used to
// offer a "view as …" cross-link from the generic source profile.
const DEX_PAGES = new Set(['soroswap', 'phoenix', 'aquarius', 'sdex', 'comet']);
const EXCHANGE_PAGES = new Set(['binance', 'coinbase', 'kraken', 'bitstamp']);

type Params = Promise<{ name: string }>;

interface VolumeBucket {
  hour: string;
  volume_usd: string;
}

interface Source {
  name: string;
  class: 'exchange' | 'aggregator' | 'oracle' | 'authority_sanity';
  subclass?: string;
  include_in_vwap: boolean;
  paid: boolean;
  backfill_available: boolean;
  backfill_safe: boolean;
  default_weight?: number;
  on_chain?: boolean;
  selectable?: boolean;
  trade_count_24h?: number;
  volume_24h_usd?: string | null;
  markets_count_24h?: number;
  volume_history_24h?: VolumeBucket[];
}

interface CursorRow {
  source: string;
  sub_source?: string;
  last_ledger: number;
  last_updated: string;
  lag_seconds: number;
}

export async function generateStaticParams() {
  // Pre-render every registered source so deep links resolve at
  // build time. CI stub falls back to `sdex` (the highest-volume
  // on-chain venue) so static export anchors on something
  // recognisable. Fail-hard (src/lib/buildFetch.ts): an unreachable
  // or empty source registry fails the build on a real host.
  const fallback = [{ name: 'sdex' }];
  const rows = requireRows(
    await buildFetchData<Source[]>('/v1/sources'),
    '/v1/sources listing for /sources/[name] static params',
  );
  const names = rows.map((s) => s.name).filter(Boolean);
  // `shell` backs functions/sources/[[path]].js for a source registered
  // after this build — see SourcePathView.
  const listed = names.length > 0 ? names.map((name) => ({ name })) : fallback;
  return [...listed, { name: 'shell' }];
}

const isShell = (name: string) => name.toLowerCase() === 'shell';

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { name } = await params;
  // One baked document answers every unlisted /sources/* path.
  if (isShell(name)) {
    return shellMetadata(
      'Source detail',
      'Per-venue source detail, rendered live from the Stellar Index API.',
    );
  }
  const canonical = `${CURRENT_NETWORK.explorerUrl}/sources/${encodeURIComponent(name)}`;
  const label = sourceLabel(name);
  const title = `${label} — source detail`;
  const description = `Per-venue source metadata, ingest cursor, and contribution profile for ${name}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: {
      title,
      description,
      url: canonical,
      type: 'website',
      images: SITE_OG_IMAGES,
    },
    twitter: {
      card: 'summary_large_image',
      title,
      description,
      images: SITE_TWITTER_IMAGES,
    },
  };
}

async function fetchSource(name: string): Promise<Source | null> {
  // Plain /v1/sources (no stats/sparkline). The unfiltered
  // call returns the full registry projection in <100ms and
  // is enough to render the registry-profile panel +
  // not-found check. Per-source 24h stats + sparkline used
  // to be in-line via include=stats,sparkline but that
  // endpoint takes 10-25s under cold-cache and was timing
  // out static-export budgets at build time. The stats now
  // load client-side via SourceStatsBlock below.
  // buildFetch memoises the listing, so every /sources/[name]
  // page of the build shares ONE registry call.
  const rows = await buildFetchData<Source[]>('/v1/sources');
  return rows?.find((s) => s.name === name) ?? null;
}

async function fetchCursors(): Promise<CursorRow[]> {
  const rows = await buildFetchData<CursorRow[]>('/v1/diagnostics/cursors');
  return rows ?? [];
}

interface MarketRow {
  base: string;
  quote: string;
  last_trade_at: string;
  trade_count_24h: number;
  volume_24h_usd?: string | null;
  last_price?: string | null;
}

async function fetchSourceMarkets(name: string): Promise<MarketRow[] | null> {
  // /v1/markets accepts ?source=<name> filter (DistinctPairs scoped
  // to one venue). Sort by volume desc and cap at 25 — the page
  // wants a "top markets" preview, not the full enumeration. An empty
  // list is legitimate (quiet venue); transport failure throws.
  //
  // `null` (CI stub / authoritative 4xx) is NOT an empty list and is
  // deliberately propagated: the panel below asserts "no markets
  // observed for this source in the trailing 14 days", and that claim
  // is only ours to bake when the API actually answered.
  return buildFetchData<MarketRow[]>(
    `/v1/markets?source=${encodeURIComponent(name)}&order_by=volume_24h_usd_desc&limit=25`,
  );
}

export default async function SourceDetailPage({ params }: { params: Params }) {
  const { name } = await params;
  if (isShell(name)) {
    return (
      <Container className="space-y-6 py-8">
        <Suspense fallback={null}>
          <SourcePathView />
        </Suspense>
      </Container>
    );
  }
  const [source, allCursors] = await Promise.all([
    fetchSource(name),
    fetchCursors(),
  ]);

  if (!source) {
    // Real build: this name was promised by generateStaticParams off
    // the SAME /v1/sources listing — a miss here means the registry
    // changed mid-build or the API broke. Fail the build rather than
    // bake "Source not found" for a real venue. The panel below
    // renders only on CI-stub builds.
    failBuild(
      `/sources/${name}: promised by generateStaticParams but /v1/sources no longer lists it`,
    );
    return (
      <EntityNotFoundShell
        title="Source not found"
        description={
          <>
            No registered source named <code className="font-mono">{name}</code>
            .
          </>
        }
        backHref="/sources"
        backLabel="All sources"
      />
    );
  }

  // The cursor's `source` is the cursor TYPE (backfill / projector /
  // ledgerstream); the venue lives in `sub_source` — projector rows
  // carry it bare ("soroswap"), backfill rows prefix a ledger range
  // ("<from-to>:sdex"). Filtering by `source === name`
  // matches nothing → the panel always read empty.
  const cursorVenue = (c: { sub_source?: string }): string => {
    const ss = (c.sub_source ?? '').trim();
    const colon = ss.lastIndexOf(':');
    return colon >= 0 ? ss.slice(colon + 1) : ss;
  };
  const cursors = allCursors.filter((c) => cursorVenue(c) === name);

  // /v1/markets?source= refuses data vendors (their prices are served only
  // blended); the API says which sources it accepts via `selectable`.
  const selectable = source.selectable === true;
  const topMarkets = selectable ? await fetchSourceMarkets(name) : null;
  const label = sourceLabel(name);

  // BreadcrumbList JSON-LD derives from the visible Crumb[] inside Breadcrumbs below.
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Sources', href: '/sources' },
          { label },
        ]}
        title={label}
        meta={
          label !== name ? <span className="font-mono">{name}</span> : undefined
        }
        actions={
          <>
            <Badge>{source.class}</Badge>
            {source.subclass && <Badge>{source.subclass}</Badge>}
            {source.paid && <Badge>Paid</Badge>}
          </>
        }
      />
      {/* Cross-link to the richer category view when one exists. */}
      {(DEX_PAGES.has(name) ||
        source.subclass === 'dex' ||
        EXCHANGE_PAGES.has(name) ||
        source.class === 'oracle') && (
        <div className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
          {(DEX_PAGES.has(name) || source.subclass === 'dex') && (
            <Link
              href={`/dexes/${encodeURIComponent(name)}`}
              className={LINK_CLASS}
            >
              View as DEX
            </Link>
          )}
          {EXCHANGE_PAGES.has(name) && (
            <Link
              href={`/exchanges/${encodeURIComponent(name)}`}
              className={LINK_CLASS}
            >
              View exchange page
            </Link>
          )}
          {source.class === 'oracle' && (
            <Link href="/oracles" className={LINK_CLASS}>
              View in oracles
            </Link>
          )}
        </div>
      )}

      <Panel
        headingLevel={2}
        title="Registry profile"
        hint="Static configuration from the source registry"
        source={asExample('/v1/sources')}
      >
        <dl className="grid grid-cols-2 gap-x-6 gap-y-4 sm:grid-cols-4">
          <Fact label="Class" value={source.class} />
          <Fact label="Subclass" value={source.subclass ?? '—'} />
          <Fact
            label="Contributes to VWAP"
            value={source.include_in_vwap ? 'yes' : 'no'}
            tone={source.include_in_vwap ? 'ok' : undefined}
          />
          <Fact
            label="Default weight"
            value={
              source.default_weight != null
                ? source.default_weight.toString()
                : '—'
            }
            mono
          />
          <Fact
            label="Backfill available"
            value={source.backfill_available ? 'yes' : 'no'}
          />
          <Fact
            label="Backfill safe (audited)"
            value={source.backfill_safe ? 'yes' : 'no'}
            tone={source.backfill_safe ? 'ok' : 'warn'}
          />
          <Fact label="Paid tier" value={source.paid ? 'yes' : 'no'} />
        </dl>
      </Panel>

      {/* Live health pane — client-side poll of /v1/sources/{name}/health
          (board #33). Registry metadata + trailing-24h liveness counters
          from the API's 15s ingestion snapshot. */}
      <SourceHealthPanel source={name} />

      {/* 24h activity panel — client-side. Was inlined SSR with
          include=stats,sparkline but that endpoint takes 10-25s
          under cold-cache and was eating the static-export budget. */}
      <SourceStatsPanel
        source={name}
        unitsLabel={sourceUnitsLabel(source.class, source.subclass)}
      />

      <SourcePeerBars source={name} />

      {selectable && <SourceTopChart source={name} sourceName={label} />}

      <Panel
        headingLevel={2}
        title="Ingest cursors"
        hint={`${cursors.length} entries · live ingest position per sub-source`}
        source={asExample('/v1/diagnostics/cursors')}
        bodyClassName={cursors.length === 0 ? undefined : '-mx-4 -mb-4'}
      >
        {cursors.length === 0 ? (
          <p className="text-ink-muted text-sm">
            No cursor recorded for this source.
          </p>
        ) : (
          // WCAG 1.4.10 Reflow: wide mono columns scroll inside the panel
          // instead of pushing the whole page sideways at 320px.
          <div className="overflow-x-auto">
            <Table>
              <THead>
                <tr>
                  <Th>Sub-source</Th>
                  <Th align="right">Last ledger</Th>
                  <Th align="right">Updated</Th>
                  <Th align="right">Lag</Th>
                </tr>
              </THead>
              <TBody>
                {cursors.map((c, i) => (
                  <TR key={`${c.sub_source ?? ''}|${i}`}>
                    <Td className="font-mono text-xs">{c.sub_source || '—'}</Td>
                    <Td align="right" className="font-mono">
                      #{c.last_ledger}
                    </Td>
                    <Td
                      align="right"
                      className="text-ink-muted font-mono text-xs"
                    >
                      {c.last_updated.replace('T', ' ').slice(0, 19)} UTC
                    </Td>
                    <Td align="right">
                      <LagBadge seconds={c.lag_seconds} />
                    </Td>
                  </TR>
                ))}
              </TBody>
            </Table>
          </div>
        )}
      </Panel>

      {selectable && (
        <Panel
          headingLevel={2}
          title="Top markets via this source"
          hint={`${topMarkets ? topMarkets.length : '—'} pairs · ranked by 24h USD volume`}
          source={asExample('/v1/markets', {
            source: name,
            order_by: 'volume_24h_usd_desc',
            limit: 25,
          })}
          bodyClassName={
            topMarkets && topMarkets.length > 0 ? '-mx-4 -mb-4' : undefined
          }
        >
          {!topMarkets ? (
            <p className="text-ink-muted text-sm">
              Market list unavailable for this build.
            </p>
          ) : topMarkets.length === 0 ? (
            <p className="text-ink-muted text-sm">
              No markets observed for this source in the trailing 14 days.
            </p>
          ) : (
            // WCAG 1.4.10 Reflow: wide mono columns scroll inside the panel
            // instead of pushing the whole page sideways at 320px.
            <div className="overflow-x-auto">
              <Table>
                <THead>
                  <tr>
                    <Th>Base</Th>
                    <Th>Quote</Th>
                    <Th align="right">Last price</Th>
                    <Th align="right">24h volume</Th>
                    <Th align="right">24h trades</Th>
                  </tr>
                </THead>
                <TBody>
                  {topMarkets.map((m) => {
                    const slug = `${m.base}~${m.quote}`;
                    const href = `/markets/${encodeURIComponent(slug)}`;
                    return (
                      <TR key={slug}>
                        <Td>
                          <Link
                            href={href}
                            className="text-ink hover:text-brand-600"
                          >
                            {shortAssetText(m.base)}
                          </Link>
                        </Td>
                        <Td>
                          <Link
                            href={href}
                            className="text-ink-muted hover:text-brand-600"
                          >
                            {shortAssetText(m.quote)}
                          </Link>
                        </Td>
                        <Td align="right" className="font-mono">
                          {formatLastPrice(m.last_price)}
                        </Td>
                        <Td align="right" className="font-mono">
                          {m.volume_24h_usd
                            ? `$${formatCompactUnits(m.volume_24h_usd)}`
                            : '—'}
                        </Td>
                        <Td align="right" className="font-mono">
                          {formatCompact(m.trade_count_24h)}
                        </Td>
                      </TR>
                    );
                  })}
                </TBody>
              </Table>
            </div>
          )}
        </Panel>
      )}
    </Container>
  );
}

const LINK_CLASS = 'text-brand-600 font-medium hover:underline';

function formatLastPrice(raw: string | null | undefined): string {
  if (!raw) return '—';
  return formatPairPrice(raw);
}

function Fact({
  label,
  value,
  mono,
  tone,
}: {
  label: string;
  value: string;
  mono?: boolean;
  tone?: 'ok' | 'warn';
}) {
  return (
    <div>
      <dt className={PANEL_LABEL_CLASS}>{label}</dt>
      <dd
        className={cn(
          'text-ink mt-1 text-sm',
          mono && 'font-mono',
          tone === 'ok' && 'text-ok-700',
          tone === 'warn' && 'text-warn-700',
        )}
      >
        {value}
      </dd>
    </div>
  );
}

function LagBadge({ seconds }: { seconds: number }) {
  const s = Math.max(0, seconds);
  const tone = s <= 60 ? 'ok' : s <= 600 ? 'warn' : 'bad';
  const label =
    s < 60
      ? `${s.toFixed(0)}s`
      : s < 3600
        ? `${(s / 60).toFixed(1)}m`
        : `${(s / 3600).toFixed(1)}h`;
  return (
    <Badge tone={tone} className="tnum">
      {label}
    </Badge>
  );
}
