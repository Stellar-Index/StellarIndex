import type { Metadata } from 'next';
import Link from 'next/link';
import { hrefFor } from '@/lib/hrefFor';
import { notFound } from 'next/navigation';

import { SourceStatsPanel } from '@/app/dexes/[source]/SourceStatsPanel';
import { ExternalRef } from '@/components/ExternalRef';
import { SITE_OG_IMAGES, SITE_TWITTER_IMAGES } from '@/lib/seo';
import { PairsTable } from './PairsTable';
import { VenueChart } from './VenueChart';

import { Badge, Container, PageHeader } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';
import { CEX_INFO } from '../registry';

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
      {/* The visible trail and its BreadcrumbList JSON-LD render from the SAME
          Crumb[] (via PageHeader → Breadcrumbs). */}
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Exchanges', href: '/exchanges' },
          { label: info.name },
        ]}
        title={info.name}
        meta={info.type}
        actions={<Badge>Curated feed</Badge>}
      />

      <p className="text-ink-muted text-sm">
        Curated feed: only the pairs that triangulate to XLM, not the full venue
        book.
      </p>

      <SourceStatsPanel source={name} unitsLabel="pairs" />

      <VenueChart venue={name} />

      <PairsTable source={name} exchangeName={info.name} />

      <div className="border-line flex flex-wrap gap-x-4 gap-y-1 border-t pt-4 text-xs">
        <Link
          href={hrefFor.source(name)}
          className="text-brand-600 font-medium hover:underline"
        >
          Source registry detail
        </Link>
        <ExternalRef href={info.homepage} />
        <ExternalRef href={info.docsUrl} />
      </div>
    </Container>
  );
}
