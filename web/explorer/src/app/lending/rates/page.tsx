import type { Metadata } from 'next';

import { Container, PageHeader } from '@/components/ui';

import { LendingRatesTable } from './LendingRatesTable';

export const metadata: Metadata = {
  alternates: { canonical: '/lending/rates' },
  title: 'Lending rates — supply and borrow APR across Stellar pools',
  description:
    'Supply and borrow APR for every reserve of every Blend and BLNT pool, side by side, with supplied and borrowed USD.',
};

export default function LendingRatesPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[
          { label: 'Home', href: '/' },
          { label: 'Lending', href: '/lending' },
          { label: 'Rates' },
        ]}
        title="Lending rates"
        meta="Supply and borrow APR for every reserve across Blend and BLNT pools. Pick an asset to compare where it earns the most."
      />
      <LendingRatesTable />
    </Container>
  );
}
