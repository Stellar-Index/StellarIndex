'use client';

import { VenueMarketsTable } from '@/components/VenueMarketsTable';

/**
 * PoolsTable — thin wrapper over the shared VenueMarketsTable (FEC audit
 * A3-F8: this file and exchanges/[name]/PairsTable were a whole-component
 * fork). The S-023 special case lives here: SDEX is an order book, so
 * 'pools' misnames its rows.
 */
export function PoolsTable({
  source,
  sourceName,
}: {
  source: string;
  sourceName: string;
}) {
  // Branch on the slug: sourceName is the display name ('SDEX').
  const orderBook = source === 'sdex';
  return (
    <VenueMarketsTable
      headingLevel={2}
      source={source}
      title={orderBook ? `${sourceName} markets` : `${sourceName} pools`}
      rowNoun={orderBook ? 'pairs' : 'pools'}
    />
  );
}
