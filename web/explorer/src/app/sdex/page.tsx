import type { Metadata } from 'next';

import { Container } from '@/components/ui';
import { ProtocolView } from '../protocols/[name]/ProtocolView';
import { SdexOrderBookSection } from './SdexOrderBookSection';
import { PoolsTable } from '../dexes/[source]/PoolsTable';
import { CURRENT_NETWORK } from '@/lib/networks';

export const metadata: Metadata = {
  alternates: { canonical: '/sdex' },
  title: 'SDEX — the Stellar Decentralized Exchange',
  description:
    'Stellar’s protocol-native central-limit order book: live verification, activity, order-book depth and volume — ingested straight from the certified ledger lake.',
  openGraph: {
    title: 'SDEX — Stellar Decentralized Exchange',
    description:
      'Stellar’s protocol-native order book: markets, offers, and trades.',
    url: `${CURRENT_NETWORK.explorerUrl}/sdex`,
    type: 'website',
  },
};

// /protocols/sdex redirects here; the protocol view already charts volume and
// trades, so this page adds only the order book and the pool table.
export default function SdexPage() {
  return (
    <>
      <ProtocolView name="sdex" label="SDEX" />
      <Container className="space-y-8 pb-10">
        <SdexOrderBookSection />
        <PoolsTable source="sdex" sourceName="SDEX" />
      </Container>
    </>
  );
}
