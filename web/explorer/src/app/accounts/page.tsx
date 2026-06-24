import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LegacyEntityRedirect } from '@/components/LegacyEntityRedirect';
import { AccountView } from './AccountView';

export const metadata: Metadata = {
  alternates: { canonical: '/accounts' },
  title: 'Accounts — Stellar accounts by wealth',
  description:
    'The richest Stellar accounts ranked by total USD holdings, plus per-account detail: balances, trustlines, offers, and sourced activity decoded straight from the certified raw lake.',
};

/**
 * /accounts — the accounts directory ranked by USD wealth, and
 * /accounts?id=G… — single-account detail (ADR-0038 Phase B/C/D).
 *
 * Query-param page (NOT an [id] dynamic route): account IDs are
 * unbounded, so under output:'export' a dynamic route would 404 on
 * any id not in generateStaticParams. The static shell hydrates and,
 * with no ?id=, renders the wealth-ranked directory (/v1/accounts);
 * with ?id=, the single-account view (/v1/accounts/{id} + its
 * transactions/operations).
 *
 * Note: this is the network-explorer account view. The customer
 * dashboard ("manage API keys") lives at the separate /account route.
 */
export default function AccountPage() {
  return (
    <Suspense fallback={null}>
      <LegacyEntityRedirect param="id" base="/accounts">
        <AccountView />
      </LegacyEntityRedirect>
    </Suspense>
  );
}
