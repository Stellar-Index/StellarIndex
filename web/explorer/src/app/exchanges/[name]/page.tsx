import type { Metadata } from 'next';
import Link from 'next/link';
import { notFound } from 'next/navigation';
import { ExternalLink } from 'lucide-react';

import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { PairsTable } from './PairsTable';
import { VenueChart } from './VenueChart';

import { Container, PageHeader } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';
const CEX_INFO: Record<
  string,
  { name: string; type: string; homepage: string; docsUrl: string; blurb: string }
> = {
  binance: {
    name: 'Binance',
    type: 'CEX — REST + WebSocket spot tickers',
    homepage: 'https://www.binance.com',
    docsUrl: 'https://github.com/binance/binance-spot-api-docs',
    blurb:
      'Spot trading pairs against XLM. We poll Binance ticker streams for trade events; usd_volume is computed Phase-1-style from USD-pegged quotes (USDT, BUSD, USDC).',
  },
  coinbase: {
    name: 'Coinbase',
    type: 'CEX — Advanced Trade WebSocket',
    homepage: 'https://www.coinbase.com',
    docsUrl: 'https://docs.cloud.coinbase.com/advanced-trade-api',
    blurb:
      'XLM spot pairs from Coinbase Advanced Trade — direct USD quote for usd_volume populates with no FX leg. The market-data feed dropped 0-quote-amount canonical-validator violations after the fix in PR #49.',
  },
  kraken: {
    name: 'Kraken',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.kraken.com',
    docsUrl: 'https://docs.kraken.com/websockets',
    blurb:
      'Kraken spot pairs against USD and EUR. Forex factor (X2.5) snaps EUR pairs into USD-equivalent volume.',
  },
  bitstamp: {
    name: 'Bitstamp',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.bitstamp.net',
    docsUrl: 'https://www.bitstamp.net/websocket/v2/',
    blurb:
      'Long-running USD-quoted XLM pairs. Smaller volume share than Binance/Coinbase but contributes to the cross-CEX VWAP weighting.',
  },
};

type Params = Promise<{ name: string }>;

export function generateStaticParams() {
  return Object.keys(CEX_INFO).map((name) => ({ name }));
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { name } = await params;
  const info = CEX_INFO[name];
  if (!info) return { title: 'Exchange not found' };
  const canonical = `${CURRENT_NETWORK.explorerUrl}/exchanges/${encodeURIComponent(name)}`;
  const title = `${info.name} — every pair, live`;
  const description = `All ${info.name} pairs observed in the last 14 days, with per-pair 24h trade count + last trade. Source: /v1/markets?source=${name}.`;
  return {
    title,
    description,
    alternates: { canonical },
    openGraph: { title, description, url: canonical, type: 'website', images: SITE_OG_IMAGES },
    twitter: { card: 'summary_large_image', title, description, images: SITE_TWITTER_IMAGES },
  };
}

export default async function ExchangeDetailPage({
  params,
}: {
  params: Params;
}) {
  const { name } = await params;
  const info = CEX_INFO[name];
  if (!info) notFound();

  return (
    <Container className="space-y-6 py-8">
      {/* FEC A1-6: the visible trail + its BreadcrumbList JSON-LD render
          from the SAME Crumb[] (via PageHeader → Breadcrumbs). This page
          used to emit hand-rolled LD with no visible crumbs. */}
      <header className="space-y-2 border-b border-line pb-4">
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Exchanges', href: '/exchanges' },
            { label: info.name },
          ]}
          eyebrow={info.type}
          title={info.name}
          description={info.blurb}
        />
        <p className="max-w-3xl rounded-md border border-warn-300 bg-warn-50 p-3 text-xs text-warn-700">
          <span className="font-semibold">Curated subscription, not a full mirror.</span>{' '}
          Stellar Index is the protocol explorer for the Stellar network, with an independent price feed; from each CEX we
          subscribe to the pairs that triangulate to XLM (the largest XLM
          markets, the BTC/ETH crypto anchors, and ~17 top-cap globals
          for cross-venue VWAP coverage). The full venue order book is
          out of scope — see the source code at{' '}
          <code className="font-mono">internal/sources/external/cex/{name}/</code>.
        </p>
      </header>

      <SourceStatsPanel source={name} unitsLabel="pairs" />

      <VenueChart venue={name} />

      <PairsTable source={name} exchangeName={info.name} />

      <div className="flex flex-wrap gap-3 text-xs">
        <Link
          href={`/sources/${name}`}
          className="inline-flex items-center gap-1 text-ink-muted hover:text-brand-600"
        >
          Source registry detail →
        </Link>
        <a
          href={info.homepage}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-ink-muted hover:underline"
        >
          {info.name} homepage
          <ExternalLink className="h-3 w-3" />
        </a>
        <a
          href={info.docsUrl}
          target="_blank"
          rel="noreferrer noopener"
          className="inline-flex items-center gap-1 text-ink-muted hover:underline"
        >
          API docs
          <ExternalLink className="h-3 w-3" />
        </a>
      </div>
    </Container>
  );
}

