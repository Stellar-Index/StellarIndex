'use client';

import { VenueMarketsTable } from '@/components/VenueMarketsTable';

/** PairsTable — thin wrapper over the shared VenueMarketsTable (FEC audit
 * A3-F8; see that component for the fold rationale). */
export function PairsTable({
  source,
  exchangeName,
}: {
  source: string;
  exchangeName: string;
}) {
  return (
    <VenueMarketsTable
      headingLevel={2}
      source={source}
      title={`${exchangeName} pairs`}
      rowNoun="pairs"
    />
  );
}
