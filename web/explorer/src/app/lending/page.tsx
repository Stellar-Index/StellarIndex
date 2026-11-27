import type { Metadata } from 'next';
import Link from 'next/link';

import { NoteBadge } from '@/components/NoteBadge';
import { Panel } from '@/components/reveal';
import { LendingPoolsTable } from './LendingPoolsTable';
import { LendingProtocolStats } from './LendingProtocolStats';
import { LendingOverviewCharts } from './LendingOverviewCharts';
import { LENDING_PROTOCOL_PAGES } from './LendingProtocolPage';
import { Container, PageHeader } from '@/components/ui';

export const metadata: Metadata = {
  alternates: { canonical: '/lending' },
  title: 'Lending — collateralised lending on Stellar',
  description:
    'Collateralised lending on Stellar: Blend and BLNT V3 pools, backstops, reserves and liquidation auctions.',
};

export default function LendingPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Lending' }]}
        title="Lending"
        meta="Collateralised lending on Stellar. Blend and BLNT V3 pools, side by side."
      />
      <nav aria-label="Lending sections" className="flex gap-4 text-sm">
        <Link href="/lending/rates" className="text-brand-600 hover:underline">
          Compare rates across pools
        </Link>
      </nav>
      <LendingOverviewCharts />
      <div className="grid gap-6 lg:grid-cols-2">
        {LENDING_PROTOCOL_PAGES.map((p) => (
          <Panel
            key={p.id}
            headingLevel={2}
            title={p.name}
            hint={p.hint}
            bodyClassName="space-y-3"
          >
            <LendingProtocolStats protocol={p.id} compact />
            <Link
              href={p.href}
              className="text-brand-600 inline-block text-sm hover:underline"
            >
              {p.name} revenue, token and pools
            </Link>
          </Panel>
        ))}
      </div>
      <LendingPoolsTable />
      <NoteBadge label="indexed pools only">
        Pools are listed once they have an indexed auction or a position; a pool
        with neither is not listed yet.
      </NoteBadge>
    </Container>
  );
}
