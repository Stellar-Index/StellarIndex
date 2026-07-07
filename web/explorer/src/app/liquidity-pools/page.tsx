import type { Metadata } from 'next';
import Link from 'next/link';

import { NativePoolsPanel } from './NativePoolsPanel';

export const metadata: Metadata = {
  title: 'Native liquidity pools on Stellar (CAP-38)',
  description:
    'Stellar’s protocol-native liquidity pools — constant-product AMM pools built into the ledger by CAP-38, distinct from Soroban AMM protocols. How they work and where to find them.',
  alternates: { canonical: '/liquidity-pools' },
  openGraph: { title: 'Native liquidity pools on Stellar', description: 'Protocol-native (CAP-38) AMM pools on Stellar.', url: 'https://stellarindex.io/liquidity-pools', type: 'website' },
};

export default function LiquidityPoolsPage() {
  return (
    <div className="mx-auto max-w-3xl space-y-8 px-6 py-10">
      <header className="space-y-3">
        <h1 className="text-3xl font-semibold tracking-tight">Native liquidity pools</h1>
        <p className="text-base text-ink-body">
          Stellar has <strong>protocol-native</strong> liquidity pools — constant-product
          automated market makers built directly into the ledger by CAP-38, settled
          deterministically at ledger close with no smart contract.
        </p>
      </header>

      <div className="rounded-xl border border-brand-200 bg-brand-50 p-5 text-sm leading-relaxed text-brand-900">
        <p>
          <strong>This section covers native pools built into the Stellar protocol.</strong>{' '}
          For AMM protocols built on Soroban (Soroswap, Aquarius, Phoenix, Comet),
          see{' '}
          <Link href="/amm" className="font-medium underline">AMM protocols</Link>.
        </p>
      </div>

      <section className="space-y-3 text-[15px] leading-relaxed text-ink-body">
        <p>
          A native pool holds a reserve of two assets and lets anyone deposit,
          withdraw, or swap against it. Path payments route through native pools
          automatically, so their prices fold into the same aggregate VWAP Stellar
          Index serves for every venue.
        </p>
        <p>
          Pool trades are captured directly from the certified ledger lake and
          appear in the{' '}
          <Link href="/markets" className="text-brand-600 hover:underline">aggregate markets</Link>.
          Each pool&apos;s two-sided reserves are read straight from its ledger
          entry in the lake — shown live below, with a constant-product depth
          estimate per pool.
        </p>
        <p>
          For Soroban AMMs, live per-pool <strong>reserve and depth</strong> views are
          available for{' '}
          <Link href="/dexes/soroswap" className="text-brand-600 hover:underline">Soroswap</Link>{' '}
          — currently the only Soroban venue whose pool-contract storage layout is
          verified against the lake. Coverage notes on each DEX page state exactly
          which venues are and aren&apos;t served.
        </p>
      </section>

      <NativePoolsPanel />
    </div>
  );
}
