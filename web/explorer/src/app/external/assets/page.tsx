import type { Metadata } from 'next';
import Link from 'next/link';
import { Suspense } from 'react';

import { Container, PageHeader, Skeleton } from '@/components/ui';
import { AssetsTable } from '../../assets/AssetsTable';

export const metadata: Metadata = {
  alternates: { canonical: '/external/assets' },
  title: 'External assets — fiat & reference prices',
  description:
    'Fiat currencies and non-Stellar reference-price coins (BTC, ETH, …) tracked for pricing. Non-Stellar assets live here; Stellar assets are on /assets.',
};

/**
 * /external/assets — the external asset directory.
 *
 * Mirrors /assets (server-component shell + Suspense client table),
 * but sources rows from `/v1/external/assets` (fiat + reference-only
 * coins — the non-Stellar side of the /v1/assets split). No verified
 * strip: that's Stellar-catalogue specific, so there's no server fetch
 * here — the client table reads `?cursor=` / `?limit=` / `?q=` /
 * `?asset_class=` from the URL exactly like /assets does.
 */
export default function ExternalAssetsPage() {
  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        eyebrow="Directory"
        title="External assets"
        description="Non-Stellar assets tracked for pricing: fiat currencies and reference-price coins (BTC, ETH, …). These are split off /assets — the Stellar-only asset directory — and priced from off-chain venues and reference feeds."
      />
      <p className="text-sm text-ink-muted">
        <Link
          href="/assets"
          className="font-medium text-brand-600 hover:text-brand-700"
        >
          ← Stellar assets
        </Link>
      </p>
      <Suspense fallback={<Skeleton className="h-96 w-full" />}>
        <AssetsTable
          endpoint="/v1/external/assets"
          basePath="/external/assets"
          classOptions={[
            { value: 'all', label: 'All' },
            { value: 'fiat', label: 'Fiat' },
            { value: 'blockchain', label: 'Crypto' },
          ]}
        />
      </Suspense>
    </Container>
  );
}
