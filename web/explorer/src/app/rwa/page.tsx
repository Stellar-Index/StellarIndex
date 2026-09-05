import { Suspense } from 'react';
import type { Metadata } from 'next';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container, PageHeader, Skeleton } from '@/components/ui';
import { RWAView } from './RWAView';

export const metadata: Metadata = {
  alternates: { canonical: '/rwa' },
  // The title says "verified issuers", not "every RWA on Stellar": this
  // page publishes only the assets whose real-world backing an
  // independent party has vouched for per (code, issuer). Claiming
  // completeness would be the same overclaim /markets had to correct.
  title: 'Real-world assets — tokenized instruments from recognised issuers',
  description:
    'Tokenized real-world assets on Stellar, identified by (code, issuer): each one carries an issuer-bound SEP-1 declaration and independent recognition of the issuing account. Valuations come from the same gates the asset pages use, and a withheld valuation shows as unavailable rather than as zero.',
};

/**
 * /rwa — tokenized real-world assets on Stellar.
 *
 * The membership rule is the product. Asset codes are not unique on
 * Stellar, so an RWA page built on codes or on issuer self-declaration
 * alone is a directory of exchange impersonators with dollar figures
 * attached — on the live network, 128 of the 130 issuers publishing a
 * domain-bound real-world `anchor_asset_type` are flagged `malicious`.
 * The server refuses those; this page renders what survives, states the
 * rule that produced it, and shows how many candidates each requirement
 * turned away.
 *
 * PageHeader stays in the server component and only the data view sits
 * inside Suspense, so the exported HTML keeps its `<h1>` and frame
 * (nav-shell.built.test.ts).
 */
export default function RWAPage() {
  // The set is built from classic assets, their issuers' SEP-1 payloads
  // and the curated account directory — the same chain shape /assets
  // and /issuers depend on. A net without those serves an empty set by
  // construction, which is a different statement from "no RWAs exist".
  if (!routeAvailable('/rwa')) {
    return (
      <Container className="space-y-8 py-8 sm:py-10">
        <PageHeader eyebrow="Tokenized instruments" title="Real-world assets" />
        <NetworkUnavailable href="/rwa" />
      </Container>
    );
  }
  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="Tokenized instruments"
        title="Real-world assets"
        description="Stellar assets representing treasuries, commodities, equities and property — admitted only when the issuer declares the anchor in a SEP-1 file served from its own on-chain domain AND an independent directory recognises that exact issuing account. Identity is always (code, issuer); a code alone identifies nothing."
      />

      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <RWAView />
      </Suspense>
    </Container>
  );
}
