import type { Metadata } from 'next';
import Link from 'next/link';
import { ExternalLink } from 'lucide-react';

import { NoteBadge } from '@/components/NoteBadge';
import { Panel } from '@/components/reveal';
import { LendingPoolsTable } from './LendingPoolsTable';
import { LendingProtocolStats } from './LendingProtocolStats';
import { LendingOverviewCharts } from './LendingOverviewCharts';
import { Badge, Container, PageHeader } from '@/components/ui';
import { protocolMeta } from '@/app/protocols/registry';

export const metadata: Metadata = {
  alternates: { canonical: '/lending' },
  title: 'Lending — collateralised lending on Stellar',
  description:
    'Collateralised lending on Stellar: Blend and BLNT V3 pools, backstops, reserves and liquidation auctions.',
};

const blnt = protocolMeta('blnt');

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
      <Panel
        headingLevel={2}
        title="Blend"
        hint="Isolated pools, priced by Reflector, liquidated through a Comet backstop"
        bodyClassName="space-y-3"
      >
        <LendingProtocolStats protocol="blend" />
        <div className="flex flex-wrap items-center gap-2">
          <Badge tone="ok" dot>
            Live
          </Badge>
          <Badge>Isolated pools</Badge>
          <Badge title="Each pool reads the SEP-40 Reflector oracle for collateral valuation.">
            Reflector-priced
          </Badge>
          <Badge title="Liquidations execute against a Comet-style auction backstop.">
            Comet backstop
          </Badge>
          <Link
            href="/protocols/blend"
            className="text-brand-600 ml-auto text-xs hover:underline"
          >
            Protocol page
          </Link>
        </div>
        <details className="text-ink-muted text-xs">
          <summary className="cursor-pointer">About Blend</summary>
          <ul className="mt-2 space-y-1">
            <li>
              Each pool is isolated (Aave-V3 style); collateral and borrow
              assets are chosen per pool by the operator.
            </li>
            <li>
              Reflector divergence from our VWAP shifts the liquidation
              threshold; it surfaces on coin pages via{' '}
              <code className="font-mono">flags.divergence_warning</code> (see{' '}
              <Link href="/oracles" className="underline decoration-dotted">
                /oracles
              </Link>
              ).
            </li>
            <li>
              The backstop is a Comet pool, the same code path indexed on{' '}
              <Link href="/dexes" className="underline decoration-dotted">
                /dexes
              </Link>
              .
            </li>
            <li>
              Liquidations can sandwich oracle updates; see{' '}
              <Link href="/mev" className="underline decoration-dotted">
                /mev
              </Link>
              .
            </li>
          </ul>
        </details>
        <a
          href="https://github.com/blend-capital/blend-contracts"
          className="text-ink-muted inline-flex items-center gap-1 text-xs hover:underline"
          target="_blank"
          rel="noreferrer"
        >
          Contracts source
          <ExternalLink className="h-3 w-3" aria-hidden="true" />
        </a>
      </Panel>
      <Panel
        headingLevel={2}
        title="BLNT V3"
        hint="Blend-derived pools with RWA support and tiered backstops"
        bodyClassName="space-y-3"
      >
        <LendingProtocolStats protocol="blnt" />
        <div className="flex flex-wrap items-center gap-2">
          <Badge>Isolated pools</Badge>
          <Badge title="Supports real-world-asset reserves.">
            RWA reserves
          </Badge>
          <Badge title="Backstop deposits sit in first-, second- and third-loss tiers.">
            Tiered backstop
          </Badge>
          <Link
            href="/protocols/blnt"
            className="text-brand-600 ml-auto text-xs hover:underline"
          >
            Protocol page
          </Link>
          <a
            href={blnt?.links?.website}
            className="text-ink-muted inline-flex items-center gap-1 text-xs hover:underline"
            target="_blank"
            rel="noreferrer"
          >
            Website
            <ExternalLink className="h-3 w-3" aria-hidden="true" />
          </a>
        </div>
      </Panel>
      <LendingPoolsTable />
      <NoteBadge label="indexed pools only">
        Pools are listed once they have an indexed auction or a position; a pool
        with neither is not listed yet.
      </NoteBadge>
    </Container>
  );
}
