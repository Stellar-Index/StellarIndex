import type { Metadata } from 'next';

import { Container, PageHeader } from '@/components/ui';
import { IssuersTable } from './IssuersTable';

export const metadata: Metadata = {
  alternates: { canonical: '/issuers' },
  title: 'Issuers — every G-account that mints classic assets on Stellar',
  description:
    'Every G-strkey that has minted a classic asset, ranked by observation count.',
};

export default function IssuersPage() {
  return (
    <Container className="space-y-8 py-8 sm:py-10">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Issuers' }]}
        title="Issuers"
        description="Classic-asset issuers ranked by observation count."
      />
      <IssuersTable />
    </Container>
  );
}
