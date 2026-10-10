import { Suspense } from 'react';
import type { Metadata } from 'next';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container, PageHeader, Skeleton } from '@/components/ui';
import { MarketsTable } from './MarketsTable';

export const metadata: Metadata = {
  alternates: { canonical: '/markets' },
  // Thousands of pairs trade on Stellar in any 14-day
  // window (see the Panel's own "top N by volume" label below) — this
  // page only ever renders the top 100 by 24h USD volume, so the title
  // and description must say that, not "every".
  title: 'Markets — top pairs by 24h volume',
  description:
    'The top 100 (base, quote) pairs by 24h USD volume that traded on Stellar in the last 14 days. Sortable, with last-trade-relative timestamps.',
};

/**
 * /markets — the top 100 active trading pairs on Stellar by 24h volume.
 *
 * v0 wires the live `/v1/markets` endpoint with a sortable table.
 * The pair-heatmap, per-venue sub-tables, and live tape (via
 * `/v1/observations/stream`) follow as their underlying data
 * surfaces stabilise.
 */
const MARKETS_CRUMBS = [{ label: 'Home', href: '/' }, { label: 'Markets' }];

export default function MarketsPage() {
  // /v1/markets is built from the aggregator's prices_1d CAGG and is
  // empty on every net without an aggregator. The page had no gate at all —
  // reachable by direct URL AND from the home network strip's "24h volume"
  // and "Active markets" tiles, and it rendered a permanently empty table.
  if (!routeAvailable('/markets')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader breadcrumbs={MARKETS_CRUMBS} title="Markets" />
        <NetworkUnavailable href="/markets" />
      </Container>
    );
  }
  return (
    <Container className="space-y-6 py-8">
      <PageHeader breadcrumbs={MARKETS_CRUMBS} title="Markets" />

      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <MarketsTable />
      </Suspense>
    </Container>
  );
}
