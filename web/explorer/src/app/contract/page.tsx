import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LegacyEntityRedirect } from '@/components/LegacyEntityRedirect';
import { ContractView } from './ContractView';

export const metadata: Metadata = {
  alternates: { canonical: '/contract' },
  title: 'Contract — Stellar contract detail',
  description:
    'Recent contract events for a single Soroban contract on Stellar — ledger, transaction, event type, and topic, straight from the certified raw lake.',
};

/**
 * /contract?id=C… — single-contract detail (ADR-0038 Phase D).
 *
 * Query-param page (NOT an [id] dynamic route): contract IDs are
 * unbounded, so under output:'export' a dynamic route would 404 on
 * any id not in generateStaticParams. The static shell hydrates and
 * reads ?id= client-side, then fetches /v1/contracts/{id}.
 */
export default function ContractPage() {
  return (
    <Suspense fallback={null}>
      <LegacyEntityRedirect param="id" base="/contracts">
        <ContractView />
      </LegacyEntityRedirect>
    </Suspense>
  );
}
