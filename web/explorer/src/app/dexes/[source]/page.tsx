import type { Metadata } from 'next';
import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { notFound } from 'next/navigation';
import { ExternalLink } from 'lucide-react';

import { Container, PageHeader } from '@/components/ui';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { DexAnalyticsSection } from './DexAnalyticsSection';
import { PairReservesPanel } from './PairReservesPanel';
import { PoolsTable } from './PoolsTable';
import { SourceStatsPanel } from './SourceStatsPanel';
import { SourceTopChart } from './SourceTopChart';
import { SourceVolumeHistory } from './SourceVolumeHistory';
import { CURRENT_NETWORK } from '@/lib/networks';
import { DEX_INFO } from '../registry';

type Params = Promise<{ source: string }>;

export function generateStaticParams() {
  return Object.keys(DEX_INFO)
    .filter((source) => source !== 'sdex')
    .map((source) => ({ source }));
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
  // SDEX is an order book: its rows are traded pairs, not pools.
  const [noun, unit] =
    source === 'sdex' ? ['market', 'pair'] : ['pool', 'pool'];
  const title = `${info.name} — every ${noun}, live`;
  const description = `All ${info.name} ${noun}s observed in the last 14 days, with per-${unit} 24h trade count + last trade. Source: /v1/markets?source=${source}.`;
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

export default async function SourceDetailPage({ params }: { params: Params }) {
  const { source } = await params;
  const info = DEX_INFO[source];
  if (!info) notFound();

  return (
    <Container className="space-y-6 py-8">
      {/* FEC A1-6: BreadcrumbList JSON-LD derives from this Crumb[] inside
          Breadcrumbs — no hand-rolled LD. */}
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'DEXes', href: '/dexes' },
          { label: info.name },
        ]}
        title={info.name}
        actions={
          <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 text-[10px] tracking-wider uppercase">
            {info.type}
          </span>
        }
      />

      <SourceStatsPanel
        source={source}
        unitsLabel={source === 'sdex' ? 'pairs' : 'pools'}
      />

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
        <p className="text-ink-muted text-xs">
          Live order-book depth for any SDEX pair is on its market page (e.g.{' '}
          <Link href="/markets" className="text-brand-600 hover:underline">
            pick a market
          </Link>{' '}
          → the SDEX order book panel) or directly via{' '}
          <code className="font-mono">/v1/sdex/orderbook</code>.
        </p>
      )}
      {source === 'soroswap' && <PairReservesPanel />}
      {source === 'aquarius' && (
        <p className="text-ink-muted text-xs">
          Aquarius per-pool reserve snapshots (latest post-state depth per pool)
          are on its{' '}
          <Link
            href="/protocols/aquarius"
            className="text-brand-600 hover:underline"
          >
            protocol analytics page
          </Link>
          ; the TVL stat above is derived from the same snapshots.
        </p>
      )}
      {source === 'sushiswap_v3' && (
        <p className="text-ink-muted text-xs">
          <span title="A concentrated-liquidity pool spreads depth across tick ranges, so its token balances are not a two-sided reserve.">
            No reserve or TVL by design (concentrated liquidity)
          </span>
          ; volume, trades and pools are complete.
        </p>
      )}
      {(source === 'phoenix' || source === 'comet') && (
        <p className="text-ink-muted text-xs">
          Per-pool reserve and depth views are currently served for{' '}
          <Link
            href="/dexes/soroswap"
            className="text-brand-600 hover:underline"
          >
            Soroswap
          </Link>{' '}
          and{' '}
          <Link
            href="/protocols/aquarius"
            className="text-brand-600 hover:underline"
          >
            Aquarius
          </Link>{' '}
          only. {info.name} has no verified reserve source yet, so no TVL is
          shown.
        </p>
      )}

      <div className="flex flex-wrap gap-3 text-xs">
        <Link
          href={hrefFor.protocol(source)}
          className="text-ink-muted hover:text-brand-600 inline-flex items-center gap-1"
        >
          Protocol analytics →
        </Link>
        <Link
          href={hrefFor.source(source)}
          className="text-ink-muted hover:text-brand-600 inline-flex items-center gap-1"
        >
          Source registry detail →
        </Link>
        {info.contractsUrl && (
          <a
            href={info.contractsUrl}
            target="_blank"
            rel="noreferrer noopener"
            className="text-ink-muted inline-flex items-center gap-1 hover:underline"
          >
            Contracts source
            <ExternalLink className="h-3 w-3" />
          </a>
        )}
      </div>
    </Container>
  );
}
