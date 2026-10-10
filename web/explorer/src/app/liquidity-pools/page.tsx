import type { Metadata } from 'next';
import Link from 'next/link';

import { Container, PageHeader } from '@/components/ui';

import { NativePoolsPanel } from './NativePoolsPanel';
import { CURRENT_NETWORK } from '@/lib/networks';

export const metadata: Metadata = {
  title: 'Native liquidity pools on Stellar (CAP-38)',
  description:
    'Protocol-native (CAP-38) constant-product liquidity pools, distinct from Soroban AMMs.',
  alternates: { canonical: '/liquidity-pools' },
  openGraph: {
    title: 'Native liquidity pools on Stellar',
    description: 'Protocol-native (CAP-38) AMM pools on Stellar.',
    url: `${CURRENT_NETWORK.explorerUrl}/liquidity-pools`,
    type: 'website',
  },
};

export default function LiquidityPoolsPage() {
  return (
    <Container className="space-y-8 py-8 sm:py-10">
      {/* Prose stays reading-width; the live pools data below gets the full frame
          (FEC A1-5: frame wide, copy narrow — the /pricing pattern). */}
      <div className="max-w-3xl space-y-8">
        <PageHeader
          breadcrumbs={[
            { label: 'Home', href: '/' },
            { label: 'Native liquidity pools' },
          ]}
          title="Native liquidity pools"
        />
        <details className="text-ink-muted text-xs">
          <summary className="cursor-pointer">About these pools</summary>
          <ul className="mt-2 space-y-1">
            <li>
              Path payments route through native pools, so their prices fold
              into the aggregate VWAP. Trades appear in the{' '}
              <Link href="/markets" className="underline">
                aggregate markets
              </Link>
              .
            </li>
            <li>
              Reserves are read from each pool&apos;s ledger entry in the lake,
              with a constant-product depth estimate.
            </li>
            <li>
              Soroban per-pool reserve and depth views exist only for{' '}
              <Link href="/dexes/soroswap" className="underline">
                Soroswap
              </Link>
              , the one venue whose pool storage layout is verified; each DEX
              page states what is served.
            </li>
          </ul>
        </details>
      </div>

      <NativePoolsPanel />
    </Container>
  );
}
