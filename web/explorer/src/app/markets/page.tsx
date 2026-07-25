import { Suspense } from 'react';
import type { Metadata } from 'next';

import { Container, PageHeader, Skeleton } from '@/components/ui';
import { MarketsTable } from './MarketsTable';

export const metadata: Metadata = {
  alternates: { canonical: '/markets' },
  // UXP-10/UXP-16: thousands of pairs trade on Stellar in any 14-day
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
export default function MarketsPage() {
  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="Trading pairs"
        title="Markets"
        description="Top 100 (base, quote) pairs by 24h USD volume, of the thousands that traded on Stellar in the last 14 days. Heatmap, per-venue sub-tables, and a live trade tape land in subsequent passes."
      />

      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <MarketsTable />
      </Suspense>
    </Container>
  );
}
