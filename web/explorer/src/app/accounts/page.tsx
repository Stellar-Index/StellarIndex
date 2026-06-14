import { Suspense } from 'react';
import type { Metadata } from 'next';

import { AccountView } from './AccountView';

export const metadata: Metadata = {
  title: 'Account — Stellar account detail',
  description:
    'Sourced/submitted activity for a single Stellar account: the transactions it submitted and the operations it sourced, decoded straight from the certified raw lake.',
};

/**
 * /accounts?id=G… — single-account detail (ADR-0038 Phase B/D).
 *
 * Query-param page (NOT an [id] dynamic route): account IDs are
 * unbounded, so under output:'export' a dynamic route would 404 on
 * any id not in generateStaticParams. The static shell hydrates and
 * reads ?id= client-side, then fetches
 * /v1/accounts/{id}/transactions + /v1/accounts/{id}/operations.
 *
 * Note: this is the network-explorer account view (sourced/submitted
 * activity). The customer dashboard ("manage API keys") lives at the
 * separate /account route.
 */
export default function AccountPage() {
  return (
    <Suspense fallback={null}>
      <AccountView />
    </Suspense>
  );
}
