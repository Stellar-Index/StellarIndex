import { Suspense } from 'react';
import type { Metadata } from 'next';

import { Container, Skeleton } from '@/components/ui';
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
 *
 * The frame (heading + standing description) is SERVER-rendered and only
 * the `?cursor=`-dependent subtree sits in Suspense, behind a VISIBLE
 * skeleton. A `<Suspense fallback={null}>` around the whole view baked
 * the null into the export — useSearchParams bails the subtree out to
 * client rendering, so under output:'export' the static document shipped
 * an empty <main> with no <h1> (guarded by lib/nav-shell.built.test.ts).
 */
export default function OperationsPage() {
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-1">
        <p className="text-ink-muted text-xs tracking-wider uppercase">
          Explorer
        </p>
        <h1 className="text-ink text-2xl font-semibold tracking-tight">
          Operations
        </h1>
        <p className="text-ink-muted max-w-2xl text-sm">
          Every operation on the network, newest first, decoded straight from
          the certified lake. Click a hash for the full transaction.
        </p>
      </header>

      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <OperationsView />
      </Suspense>
    </Container>
  );
}
