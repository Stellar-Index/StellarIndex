import { Suspense } from 'react';
import type { Metadata } from 'next';

import { OperationsView } from './OperationsView';

export const metadata: Metadata = {
  alternates: { canonical: '/operations' },
  title: 'Operations — Stellar network operations',
  description:
    'Every operation on the Stellar network, newest first, decoded straight from the certified raw lake, with a trailing-24h breakdown by operation type.',
};

/**
 * /operations — the network-wide recent-operations directory
 * (GET /v1/operations, no ledger param). Keyset-paged via ?cursor=.
 * The static shell hydrates and reads ?cursor= client-side.
 */
export default function OperationsPage() {
  return (
    <Suspense fallback={null}>
      <OperationsView />
    </Suspense>
  );
}
