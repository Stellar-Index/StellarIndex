import type { Metadata } from 'next';

import { ProtocolsIndex } from '../protocols/ProtocolsIndex';

export const metadata: Metadata = {
  title: 'Yield protocols on Stellar — vaults & strategies',
  description:
    'Yield protocols on Stellar (Soroban): DeFindex and others — vaults, strategies, and the assets they deploy across Stellar DeFi. Per-protocol contract roster, event distribution, and verified-completeness verdict. Source: /v1/protocols.',
  alternates: { canonical: '/yield' },
  openGraph: { title: 'Stellar yield protocols', description: 'Vaults and strategies on Stellar.', url: 'https://stellarindex.io/yield', type: 'website' },
};

export default function YieldPage() {
  return (
    <ProtocolsIndex
      lockedCategory="yield"
      eyebrow="Yield"
      title="Yield protocols"
      description="Yield vaults and strategies on Stellar — Soroban contracts that route deposits across DeFi for return. Each protocol page carries its full contract roster, the distribution of every event type it emits, live vault-flow analytics, and a verified-completeness verdict against the certified ledger lake."
    />
  );
}
