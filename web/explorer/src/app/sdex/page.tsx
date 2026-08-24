import type { Metadata } from 'next';

import { Container } from '@/components/ui';
import { ProtocolView } from '../protocols/[name]/ProtocolView';
import { SdexOrderBookSection } from './SdexOrderBookSection';
import { SdexVolumeSection } from './SdexVolumeSection';

export const metadata: Metadata = {
  alternates: { canonical: '/sdex' },
  title: 'SDEX — the Stellar Decentralized Exchange',
  description:
    'Stellar’s protocol-native central-limit order book: live verification, activity, order-book depth and volume — ingested straight from the certified ledger lake.',
  openGraph: {
    title: 'SDEX — Stellar Decentralized Exchange',
    description:
      'Stellar’s protocol-native order book: markets, offers, and trades.',
    url: 'https://stellarindex.io/sdex',
    type: 'website',
  },
};

// Nav revision follow-up (2026-08-24): /sdex is the ONE canonical SDEX
// surface. It renders the protocol data view (the same component the
// /protocols/[name] route uses — verification, TVL/activity, freshness)
// plus the SDEX-only live sections (order-book depth + daily volume).
// /protocols/sdex permanently redirects here so the two never diverge.
export default function SdexPage() {
  return (
    <>
      <ProtocolView name="sdex" label="SDEX" />
      <Container className="space-y-8 pb-10">
        <SdexOrderBookSection />
        <SdexVolumeSection />
      </Container>
    </>
  );
}
