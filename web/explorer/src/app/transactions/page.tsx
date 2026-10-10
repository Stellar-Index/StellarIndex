import { Suspense } from 'react';
import type { Metadata } from 'next';

import { Container, PageHeader, Skeleton } from '@/components/ui';
import { TransactionsView } from './TransactionsView';

export const metadata: Metadata = {
  title: 'Transactions — recent Stellar network activity',
  description:
    'Recent Stellar transactions, newest ledger first — source account, operation count, result, fee, and memo. Click through for the full decoded transaction. Source: /v1/ledgers/{seq}/transactions.',
  alternates: { canonical: '/transactions' },
};

/**
 * /transactions — recent network transactions, `?seq=N` pins a ledger.
 *
 * The frame (heading + standing description) is SERVER-rendered and the
 * `?seq=`-dependent subtree alone sits in Suspense, behind a VISIBLE
 * skeleton. A `<Suspense fallback={null}>` around the
 * whole view would break: useSearchParams bails the subtree out to client rendering,
 * so under output:'export' the null fallback is what got baked and the
 * static document shipped an empty <main> with no <h1> — nothing for a
 * crawler, a no-JS reader, or the first paint. Only useSearchParams
 * CHILDREN may be Suspense-wrapped here, and the fallback must be real
 * content (guarded by lib/nav-shell.built.test.ts).
 */
export default function TransactionsPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Transactions' }]}
        title="Transactions"
        description="Recent transactions, newest ledger first. Click a hash for the full decoded transaction — operations, events, and result codes."
      />

      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <TransactionsView />
      </Suspense>
    </Container>
  );
}
