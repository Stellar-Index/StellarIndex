import type { Metadata } from 'next';
import { Suspense } from 'react';

import { ButtonLink, Container, PageHeader, Skeleton } from '@/components/ui';
import { AssetsTable } from './AssetsTable';
import { VerifiedStrip } from './VerifiedStrip';
import { fetchVerifiedCurrencies } from './verified-currencies';

export const metadata: Metadata = {
  alternates: { canonical: '/assets' },
  title: 'Assets — every token on Stellar',
  description:
    'Browse every classic and Soroban asset observed on Stellar — live price, 24h volume, market cap, supply, issuer. The canonical Stellar asset directory.',
};

/**
 * /assets — the explorer's asset directory.
 *
 * Server-component shell wraps a client-side table in Suspense so
 * the static export can pre-render the page chrome while the
 * client reads `?cursor=` / `?limit=` / `?issuer=` from the URL.
 */
export default async function AssetsPage() {
  // Single server-side fetch of the verified-currency catalogue
  // shared between the strip (renders each entry as a chip) and the
  // table (marks each row whose slug is in the catalogue with a
  // green check). One round-trip, no double fetch.
  const verified = await fetchVerifiedCurrencies();
  const verifiedSlugs = verified.map((v) => v.slug);

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Assets' }]}
        title="Assets"
        meta="Every classic and Soroban asset observed on Stellar, with its independent price, 24h volume and supply."
        actions={
          <ButtonLink href="/external/assets" variant="secondary">
            External assets
          </ButtonLink>
        }
      />
      <VerifiedStrip items={verified} />
      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <AssetsTable verifiedSlugs={verifiedSlugs} />
      </Suspense>
    </Container>
  );
}
