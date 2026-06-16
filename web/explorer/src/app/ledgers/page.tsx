import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LedgersTable } from './LedgersTable';

export const metadata: Metadata = {
  title: 'Ledgers — recent Stellar ledgers',
  description:
    'Every recent ledger close on the Stellar network — sequence, close time, transaction / operation counts, and Soroban event counts, straight from the certified raw lake.',
};

/**
 * /ledgers — recent ledgers list (ADR-0038 Phase D).
 *
 * Normal static page: the LIST is bounded enough to render as a
 * single page that fetches the most recent ledgers client-side from
 * /v1/ledgers. Individual ledgers are unbounded, so the per-ledger
 * view lives at /ledger?seq=N (a query-param page), not a [seq]
 * dynamic route.
 */
export default function LedgersPage() {
  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">Ledgers</h1>
        <p className="max-w-3xl text-sm text-slate-600">
          The most recent ledger closes on the Stellar network. Each row links
          to the full ledger view — header, transactions, and the operations +
          events those transactions emitted.
        </p>
      </header>

      <Suspense fallback={null}>
        <LedgersTable />
      </Suspense>
    </div>
  );
}
