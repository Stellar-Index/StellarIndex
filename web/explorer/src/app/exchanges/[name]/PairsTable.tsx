'use client';

import { VenueMarketsTable } from '@/components/VenueMarketsTable';

/** PairsTable — thin wrapper over the shared VenueMarketsTable. */
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
