import type { Metadata } from 'next';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ExternalLink } from 'lucide-react';

import { Container, Breadcrumbs } from '@/components/ui';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { DexAnalyticsSection } from './DexAnalyticsSection';
import { PairReservesPanel } from './PairReservesPanel';
import { PoolsTable } from './PoolsTable';
import { SourceStatsPanel } from './SourceStatsPanel';
import { SourceTopChart } from './SourceTopChart';
import { SourceVolumeHistory } from './SourceVolumeHistory';
import { CURRENT_NETWORK } from '@/lib/networks';

// Curated list of DEX sources with friendly names + audit links.
// Mirrors the DEX cards on /dexes; per-DEX detail pages are
// statically pre-rendered for these slugs only. New DEXes added
// here automatically get a /dexes/<source> page.
//
// A Subclass=DEX source MISSING from this map is not a missing page but
// a broken one: sitemap.ts emits /dexes/<name> for every subclass=dex
// source it reads off /v1/sources, so the URL is published and then
// 404s. scripts/ci/lint-protocol-registry-sync.sh §2 cross-checks this
// map (and ALL_DEXES in DexesView) against the Go source registry.
const DEX_INFO: Record<
  string,
  { name: string; type: string; status: string; contractsUrl?: string; blurb: string }
> = {
  soroswap: {
    name: 'Soroswap',
    type: 'Uniswap V2 clone (Soroban)',
    status: 'live',
    contractsUrl: 'https://github.com/soroswap/core',
    blurb:
      'Constant-product AMM. Each pool below is a SoroswapPair contract. Click a pool to drill into its trade history and live VWAP.',
  },
  phoenix: {
    name: 'Phoenix',
    type: 'AMM (Soroban)',
    status: 'live',
    blurb:
      'Soroban AMM with per-field event split. Each pool below is one Phoenix pair contract.',
  },
  aquarius: {
    name: 'Aquarius',
    type: 'AMM with gauges (Soroban)',
    status: 'live',
    blurb:
      'Curve-style AMM with bribe/gauge layer. Constant-product and stableswap pools render uniformly.',
  },
  sdex: {
    name: 'SDEX',
    type: 'Native order book (classic)',
    status: 'native',
    blurb:
      'Stellar-native on-chain order book. Each row below is a (base, quote) classic-asset pair that traded on SDEX in the recency window.',
  },
  comet: {
    name: 'Comet',
    type: 'Balancer V1 fork (Soroban)',
    status: 'experimental',
    blurb:
      'Balancer-style multi-asset pool. Shared ("POOL", <event>) topic across every Comet pool contract.',
  },
  sushiswap_v3: {
    name: 'SushiSwap V3',
    type: 'Concentrated liquidity (Soroban)',
    status: 'live',
    blurb:
      'Concentrated-liquidity AMM, factory-gated on a single pool factory. Each pool below is one V3 pool contract. Depth sits in per-position tick ranges rather than one two-sided reserve, so this venue carries no reserve or TVL figure — see the note under the table.',
  },
};

type Params = Promise<{ source: string }>;

export function generateStaticParams() {
  return Object.keys(DEX_INFO).map((source) => ({ source }));
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { source } = await params;
  const info = DEX_INFO[source];
  if (!info) return { title: 'DEX not found' };
  const canonical = `${CURRENT_NETWORK.explorerUrl}/dexes/${encodeURIComponent(source)}`;
  const title = `${info.name} — every pool, live`;
  const description = `All ${info.name} pools observed in the last 14 days, with per-pool 24h trade count + last trade. Source: /v1/markets?source=${source}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: { title, description, url: canonical, type: 'website', images: SITE_OG_IMAGES },
    twitter: { card: 'summary_large_image', title, description, images: SITE_TWITTER_IMAGES },
  };
}

export default async function SourceDetailPage({
  params,
}: {
  params: Params;
}) {
  const { source } = await params;
  const info = DEX_INFO[source];
  if (!info) notFound();

  return (
    <Container className="space-y-6 py-8">
      {/* FEC A1-6: BreadcrumbList JSON-LD derives from this Crumb[] inside
          Breadcrumbs — no hand-rolled LD. */}
      <Breadcrumbs
        items={[
          { label: 'Home', href: '/' },
          { label: 'DEXes', href: '/dexes' },
          { label: info.name },
        ]}
      />

      <header className="space-y-2 border-b border-line pb-4">
        <div className="flex flex-wrap items-baseline gap-3">
          <h1 className="text-3xl font-semibold tracking-tight">
            {info.name}
          </h1>
          <span className="rounded-sm bg-surface-subtle px-1.5 py-0.5 text-[10px] uppercase tracking-wider text-ink-body">
            {info.type}
          </span>
        </div>
        <p className="max-w-3xl text-sm text-ink-body">
          {info.blurb}
        </p>
      </header>

      <SourceStatsPanel source={source} />

      {/* Per-DEX bespoke analytics suite (KPIs, trades/traders series,
          top-pairs multi-line, volume-by-pair donut) — the same
          /v1/protocols/{source} block the protocol page renders; absent
          when the API serves no bespoke block. */}
      <DexAnalyticsSection source={source} />

      <SourceTopChart source={source} sourceName={info.name} />

      {/* 90d daily USD volume — the protocol-analytics aggregate,
          reused (renders nothing when the protocol has no series). */}
      <SourceVolumeHistory source={source} />

      <PoolsTable source={source} sourceName={info.name} />

      {source === 'sdex' && (
        <p className="text-xs text-ink-muted">
          Live order-book depth for any SDEX pair is on its market page
          (e.g.{' '}
          <Link href="/markets" className="text-brand-600 hover:underline">
            pick a market
          </Link>{' '}
          → the SDEX order book panel) or directly via{' '}
          <code className="font-mono">/v1/sdex/orderbook</code>.
        </p>
      )}
      {source === 'soroswap' && <PairReservesPanel />}
      {source === 'aquarius' && (
        <p className="text-xs text-ink-muted">
          Aquarius per-pool reserve snapshots (latest post-state depth per
          pool) are on its{' '}
          <Link href="/protocols/aquarius" className="text-brand-600 hover:underline">
            protocol analytics page
          </Link>
          ; the TVL stat above is derived from the same snapshots.
        </p>
      )}
      {source === 'sushiswap_v3' && (
        <p className="text-xs text-ink-muted">
          No reserve or TVL figure is served for {info.name}, and that is a
          methodology decision rather than a gap: a concentrated-liquidity pool
          spreads its depth across per-position tick ranges, so the pool&apos;s
          token balances are not a two-sided reserve and summing them would
          answer a different question than the one asked. Running the
          constant-product path over them anyway would put a meaningless number
          on this page, so none is derived —{' '}
          <code className="font-mono">/v1/protocols/sushiswap_v3/tvl</code>{' '}
          says the same thing, and the venue is named in the{' '}
          <Link href="/dexes" className="text-brand-600 hover:underline">
            headline TVL
          </Link>
          &apos;s <code className="font-mono">excluded</code> list. Swap volume,
          trades and pools above are complete.
        </p>
      )}
      {(source === 'phoenix' || source === 'comet') && (
        <p className="text-xs text-ink-muted">
          Per-pool reserve and depth views are currently served for{' '}
          <Link href="/dexes/soroswap" className="text-brand-600 hover:underline">Soroswap</Link>{' '}
          and{' '}
          <Link href="/protocols/aquarius" className="text-brand-600 hover:underline">Aquarius</Link>{' '}
          only — {info.name} emits liquidity flows, not post-state reserves, and its
          pool-storage layout hasn&apos;t been verified against the ledger lake yet.
          We don&apos;t serve guesses, so it also carries no TVL figure.
        </p>
      )}

      <div className="flex flex-wrap gap-3 text-xs">
        <Link
          href={`/protocols/${source}`}
          className="inline-flex items-center gap-1 text-ink-muted hover:text-brand-600"
        >
          Protocol analytics →
        </Link>
        <Link
          href={`/sources/${source}`}
          className="inline-flex items-center gap-1 text-ink-muted hover:text-brand-600"
        >
          Source registry detail →
        </Link>
        {info.contractsUrl && (
          <a
            href={info.contractsUrl}
            target="_blank"
            rel="noreferrer noopener"
            className="inline-flex items-center gap-1 text-ink-muted hover:underline"
          >
            Contracts source
            <ExternalLink className="h-3 w-3" />
          </a>
        )}
      </div>
    </Container>
  );
}

