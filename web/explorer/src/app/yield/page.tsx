import type { Metadata } from 'next';

import { ProtocolsIndex } from '../protocols/ProtocolsIndex';
import { CURRENT_NETWORK } from '@/lib/networks';

export const metadata: Metadata = {
  title: 'Yield protocols on Stellar — vaults & strategies',
  description:
    'Yield protocols on Stellar (Soroban): DeFindex and others — vaults, strategies, and the assets they deploy across Stellar DeFi. Per-protocol contract roster, event distribution, and verified-completeness verdict. Source: /v1/protocols.',
  alternates: { canonical: '/yield' },
  openGraph: {
    title: 'Stellar yield protocols',
    description: 'Vaults and strategies on Stellar.',
    url: `${CURRENT_NETWORK.explorerUrl}/yield`,
    type: 'website',
  },
};

export default function YieldPage() {
  return <ProtocolsIndex lockedCategory="yield" title="Yield protocols" />;
}
