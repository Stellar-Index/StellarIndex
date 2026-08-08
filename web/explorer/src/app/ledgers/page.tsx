import type { Metadata } from 'next';

import { LedgersTable } from './LedgersTable';
import { ThroughputPanel } from '@/components/NetworkInsight';

export const metadata: Metadata = {
  alternates: { canonical: '/ledgers' },
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
        <p className="max-w-3xl text-sm text-ink-body">
          The most recent ledger closes on the Stellar network. Each row links
          to the full ledger view — header, transactions, and the operations +
          events those transactions emitted.
        </p>
      </header>

      {/* S-005: ledger cadence answers "is the network healthy" before
          the row list — same shared series /network renders. */}
      <ThroughputPanel defaultMetric="ledgers" />

      {/* NO Suspense wrapper (2026-08-09): LedgersTable takes no
          useSearchParams, so a <Suspense fallback={null}> around it was
          vestigial — and actively harmful: the static exporter emitted
          the boundary as a NEVER-COMPLETING pending template
          (<!--$?--><template id="B:0">), so the browser never hydrated
          or client-rendered the subtree and the table sat on
          "Loading…" forever with zero network activity. Every other
          page's Suspense child either uses useSearchParams (CSR
          bailout — resolves client-side) or completes at export time;
          this was the only pending boundary shipped on the site. Wrap
          a client component in Suspense here ONLY if it calls
          useSearchParams. */}
      <LedgersTable />
    </div>
  );
}
