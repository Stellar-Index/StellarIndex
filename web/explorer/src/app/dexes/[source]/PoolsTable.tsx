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
  return (
    <VenueMarketsTable
      headingLevel={2}
      source={source}
      title={sourceName === 'sdex' ? 'SDEX markets' : `${sourceName} pools`}
      rowNoun={sourceName === 'sdex' ? 'pairs' : 'pools'}
    />
  );
}
