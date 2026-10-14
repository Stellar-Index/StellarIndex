import type { Metadata } from 'next';
import { Suspense } from 'react';
import { ArrowUpRight } from 'lucide-react';

import { ButtonLink, Container, PageHeader, Skeleton } from '@/components/ui';
import { AssetsTable } from './AssetsTable';
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
  // The table marks each row whose slug is in the verified catalogue.
  const verified = await fetchVerifiedCurrencies();
  const verifiedSlugs = verified.map((v) => v.slug);

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Assets' }]}
        title="Assets"
        actions={
          <ButtonLink href="/external/assets" variant="secondary" size="sm">
            External assets
            <ArrowUpRight className="h-3.5 w-3.5" aria-hidden="true" />
          </ButtonLink>
        }
      />
      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <AssetsTable verifiedSlugs={verifiedSlugs} />
      </Suspense>
    </Container>
  );
}
