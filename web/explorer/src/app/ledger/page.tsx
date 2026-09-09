import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LegacyEntityRedirect } from '@/components/LegacyEntityRedirect';
import { LedgerView } from './LedgerView';

export const metadata: Metadata = {
  alternates: { canonical: '/ledger' },
  title: 'Ledger — Stellar ledger detail',
  description:
    'Full detail for a single Stellar ledger: header (hashes, protocol version, total coins, fee pool) and every transaction it closed.',
  // noindex, like the canonical /ledgers/[seq] it redirects to — see the
  // note on /contract: a ?seq=-driven shell whose bare, self-canonical
  // URL is the only form a crawler can build.
  robots: { index: false, follow: true },
};

/**
 * /ledger?seq=N — single-ledger detail (ADR-0038 Phase D).
 *
 * Query-param page (NOT a [seq] dynamic route): ledger sequences are
 * unbounded, so under output:'export' a dynamic route would 404 on
 * any seq not in generateStaticParams. The static shell hydrates and
 * reads ?seq= client-side, then fetches /v1/ledgers/{seq} +
 * /v1/ledgers/{seq}/transactions.
 */
export default function LedgerPage() {
  return (
    <Suspense fallback={null}>
      <LegacyEntityRedirect param="seq" base="/ledgers">
        <LedgerView />
      </LegacyEntityRedirect>
    </Suspense>
  );
}
