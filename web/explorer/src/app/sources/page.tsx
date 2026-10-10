import { DataTrustTabs } from '@/components/nav/DataTrustTabs';
import type { Metadata } from 'next';
import Link from 'next/link';
import { SourcesTable } from './SourcesTable';

import { Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/sources' },
  title: 'Stellar sources — every on-chain venue we index',
  description:
    'Live registry of the Stellar on-chain sources we index, grouped by class (DEX / oracle / lending / router / bridge). Only DEX-class contributes to VWAP by default.',
};

/**
 * /sources — directory of every venue we ingest, grouped by class so the
 * VWAP boundary is visible. Per-source health lives on /sources/[name].
 */
export default function SourcesPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Sources' }]}
        title="Stellar sources"
        description={
          <>
            On-chain venues by class; only DEXes feed VWAP. Off-chain feeds are
            under{' '}
            <Link href="/exchanges" className="text-brand-600 hover:underline">
              exchanges
            </Link>
            .
          </>
        }
      />

      <DataTrustTabs active="/sources" />

      <SourcesTable />
    </Container>
  );
}
