import type { Metadata } from 'next';

import { ContractsView } from './ContractsView';

export const metadata: Metadata = {
  title: 'Contracts — active Soroban contracts on Stellar',
  description:
    'The most active Soroban contracts over a recent window, ranked by emitted events and tagged with their owning protocol where known. Click through for each contract’s events, decoded code, and cross-contract interaction map. Source: /v1/contracts.',
  alternates: { canonical: '/contracts' },
};

export default function ContractsPage() {
  return <ContractsView />;
}
