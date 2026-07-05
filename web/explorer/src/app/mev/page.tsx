import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';

import { MevFeed } from './MevFeed';

export const metadata: Metadata = {
  title: 'MEV — on-chain MEV detector',
  description:
    'MEV patterns detected on Stellar: atomic arbitrage, sandwich, oracle-update sandwich, liquidation cascade and wash trading — with honest, evidence-first detection notes.',
  alternates: { canonical: '/mev' },
};

const PATTERNS: { name: string; kind: string; description: string; caveat: string }[] = [
  {
    name: 'Atomic arbitrage',
    kind: 'arbitrage',
    description:
      'One taker trades a closed asset cycle (≥2 legs returning to the starting asset) inside a single transaction, across pools/venues. The structure itself is the evidence.',
    caveat: 'Profit is not estimated — leg direction is ambiguous in the served rows.',
  },
  {
    name: 'Sandwich',
    kind: 'sandwich',
    description:
      "One account's trades in two different transactions bracket another account's trade on the same pair within one ledger. Transaction application order (tx_index) comes from the raw ledger lake — the signal the served rows don't carry.",
    caveat:
      'Positional signature only: front/back direction opposition is not verified, so a bracketing DCA bot can look identical. Candidates, not verdicts.',
  },
  {
    name: 'Oracle-update sandwich',
    kind: 'oracle_sandwich',
    description:
      "One account's trades sit on BOTH sides (by tx_index) of an on-chain oracle update for an asset those trades touch, all within a single ledger — e.g. positioning around a Reflector price write.",
    caveat:
      'The trade/update relationship is timing evidence, not proven profitability.',
  },
  {
    name: 'Liquidation cascade',
    kind: 'liquidation_cascade',
    description:
      'A Blend liquidation-auction fill following another fill against a different position within a 12-ledger window, with an on-chain oracle update inside the bracket.',
    caveat:
      'Correlation, not causality: the cluster + oracle timing is recorded; "the first liquidation moved the price" is not proven.',
  },
  {
    name: 'Wash trading',
    kind: 'wash_trade',
    description:
      'Self-crosses (the same account is maker AND taker of one trade) and round trips (two accounts repeatedly filling each other in both directions on one pair within a UTC day).',
    caveat:
      'Round trips are also what tight two-party market-making looks like; self-crosses are unambiguous.',
  },
];

export default function MevPage() {
  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">MEV</h1>
        <p className="max-w-3xl text-sm text-ink-body">
          On-chain MEV detector. Five patterns are detected live: atomic
          arbitrage and wash trading from the canonical trade stream,
          liquidation cascades from Blend auctions correlated with oracle
          updates, and sandwich / oracle-update sandwich using intra-ledger
          transaction ordering (tx_index) resolved from the raw ledger
          lake. Every event records evidence plus a note stating exactly
          what is — and is not — claimed.
        </p>
      </header>

      <MevFeed />

      <Panel
        title="What we look for"
        hint="Pattern catalogue — evidence-first, false-positive-tolerant"
      >
        <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
          {PATTERNS.map((p) => (
            <div
              key={p.name}
              className="rounded-lg border border-line bg-surface-muted p-3 text-xs"
            >
              <div className="flex items-center justify-between gap-2">
                <h3 className="text-sm font-semibold">{p.name}</h3>
                <span className="inline-block rounded-sm bg-up-subtle px-1.5 py-0.5 text-[10px] font-medium uppercase tracking-wider text-up-strong">
                  live
                </span>
              </div>
              <p className="mt-1 text-ink-body">{p.description}</p>
              <p className="mt-2 italic text-ink-muted">{p.caveat}</p>
            </div>
          ))}
        </div>
      </Panel>

      <Panel
        title="Why this matters for pricing"
        bodyClassName="text-sm text-ink-body space-y-2"
      >
        <p>
          MEV trades show up as ordinary swaps on the wire. Without
          detection, a sandwich pair would inflate observation count
          on the same pair the victim contributed to, and an oracle-
          update sandwich would skew the oracle reading the
          liquidation price was set against.
        </p>
        <p>
          Detected events get a per-trade flag in{' '}
          <code className="font-mono text-xs">mev_events</code>{' '}
          (migration 0021). The aggregator can then optionally exclude
          flagged trades from VWAP — the policy lever lives at the
          aggregator, not the decoder, so we keep the raw observation
          and let downstream methodology decide.
        </p>
      </Panel>

      <Panel
        title="Known limits"
        bodyClassName="text-sm text-ink-body space-y-2"
      >
        <p>
          Detection is deliberately conservative about what it claims.
          The served trade rows carry no direction (buy vs sell), so no
          detector asserts front-run/back-run intent or estimates
          attacker profit — <code className="font-mono text-xs">profit_usd</code>{' '}
          is always null and each event&apos;s{' '}
          <code className="font-mono text-xs">detail.note</code> says what
          the evidence actually shows. Sandwich kinds depend on the
          lake&apos;s transaction-order index and degrade to not-detected
          (never guessed) when a transaction isn&apos;t indexed yet. An{' '}
          <code className="font-mono text-xs">oracle_deviation</code> kind
          remains reserved. Sub-invocation call-tree attribution (who
          invoked whom inside a transaction) awaits diagnostic-event
          capture in the lake. For the underlying methodology see the{' '}
          <Link href="/research" className="underline decoration-dotted">
            research index
          </Link>
          .
        </p>
      </Panel>
    </div>
  );
}
