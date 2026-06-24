import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LegacyEntityRedirect } from '@/components/LegacyEntityRedirect';
import { TxView } from './TxView';

export const metadata: Metadata = {
  alternates: { canonical: '/tx' },
  title: 'Transaction — Stellar transaction detail',
  description:
    'Full detail for a single Stellar transaction: summary, decoded operations, and the contract events it emitted.',
};

/**
 * /tx?hash=H — single-transaction detail (ADR-0038 Phase D).
 *
 * Query-param page (NOT a [hash] dynamic route): tx hashes are
 * unbounded, so under output:'export' a dynamic route would 404 on
 * any hash not in generateStaticParams. The static shell hydrates and
 * reads ?hash= client-side, then fetches /v1/tx/{hash}.
 */
export default function TxPage() {
  return (
    <Suspense fallback={null}>
      <LegacyEntityRedirect param="hash" base="/transactions">
        <TxView />
      </LegacyEntityRedirect>
    </Suspense>
  );
}
