import type { Metadata } from 'next';
import Link from 'next/link';
import { ExternalLink } from 'lucide-react';

import { NoteBadge } from '@/components/NoteBadge';
import { LendingPoolsTable } from './LendingPoolsTable';

import { Badge, Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/lending' },
  title: 'Lending — collateralised lending on Stellar',
  description:
    'Blend is the primary collateralised-lending protocol on Stellar. Backstop pools, Reflector dependency, MEV-relevant liquidations.',
};

export default function LendingPage() {
  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Lending' }]}
        title="Lending"
      />

      <div className="border-line bg-surface rounded-xl border p-5 shadow-sm">
        <div className="flex flex-wrap items-center gap-2">
          <h2 className="text-h3 text-ink font-semibold">Blend</h2>
          <Badge tone="up">Live</Badge>
          <Badge>Isolated pools</Badge>
          <Badge title="Each pool reads the SEP-40 Reflector oracle for collateral valuation.">
            Reflector-priced
          </Badge>
          <Badge title="Liquidations execute against a Comet-style auction backstop.">
            Comet backstop
          </Badge>
          <a
            href="https://github.com/blend-capital/blend-contracts"
            className="text-ink-muted ml-auto inline-flex items-center gap-1 text-xs hover:underline"
            target="_blank"
            rel="noreferrer"
          >
            Contracts source
            <ExternalLink className="h-3 w-3" />
          </a>
        </div>
        <details className="text-ink-muted mt-3 text-xs">
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
      </div>

      <LendingPoolsTable />

      <NoteBadge label="partial: pools with auctions only">
        Only pools that have had an auction are listed.
      </NoteBadge>
    </Container>
  );
}
