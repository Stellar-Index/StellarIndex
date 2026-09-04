import { Suspense } from 'react';
import type { Metadata } from 'next';

import { LegacyEntityRedirect } from '@/components/LegacyEntityRedirect';
import { Container, Skeleton } from '@/components/ui';
import { CURRENT_NETWORK } from '@/lib/networks';
import { AccountsDirectoryBody, AccountsDirectoryHeader } from './AccountView';

export const metadata: Metadata = {
  alternates: { canonical: '/accounts' },
  title: 'Accounts — Stellar accounts by wealth',
  description: CURRENT_NETWORK.pricing
    ? 'The richest Stellar accounts ranked by total USD holdings, plus per-account detail: balances, trustlines, offers, and sourced activity decoded straight from the certified raw lake.'
    : `The largest Stellar ${CURRENT_NETWORK.label} accounts ranked by native XLM balance, plus per-account detail: balances, trustlines, offers, and sourced activity decoded straight from the certified raw lake.`,
};

/**
 * /accounts — the accounts directory ranked by USD wealth. Legacy
 * /accounts?id=G… redirects to the /accounts/{g} path route (ADR-0038
 * Phase B/C/D); account IDs are unbounded, so a dynamic route would 404
 * under output:'export' on any id not in generateStaticParams.
 *
 * The frame (heading + standing description) is SERVER-rendered and only
 * the URL-dependent subtree sits in Suspense, behind a VISIBLE skeleton.
 * A `<Suspense fallback={null}>` around the whole page baked the null
 * into the export — LegacyEntityRedirect reads `?id=` through
 * useSearchParams, which bails the subtree out to client rendering, so
 * the static document shipped an empty <main> with no <h1> (guarded by
 * lib/nav-shell.built.test.ts).
 *
 * Note: this is the network-explorer account view. The customer
 * dashboard ("manage API keys") lives at the separate /account route.
 */
export default function AccountPage() {
  return (
    <Container className="space-y-6 py-8">
      <AccountsDirectoryHeader />
      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <LegacyEntityRedirect param="id" base="/accounts">
          <AccountsDirectoryBody />
        </LegacyEntityRedirect>
      </Suspense>
    </Container>
  );
}
