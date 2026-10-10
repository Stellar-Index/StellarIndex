import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';

import { MevFeed } from './MevFeed';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Badge, Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  title: 'MEV — on-chain MEV detector',
  description:
    'MEV patterns detected on Stellar: atomic arbitrage, sandwich, oracle-update sandwich, liquidation cascade and wash trading — with honest, evidence-first detection notes.',
  alternates: { canonical: '/mev' },
};

const PATTERNS: {
  name: string;
  kind: string;
  description: string;
  caveat: string;
}[] = [
  {
    name: 'Atomic arbitrage',
    kind: 'arbitrage',
    description:
      'One taker trades a closed asset cycle (≥2 legs returning to the starting asset) inside a single transaction, across pools/venues. The structure itself is the evidence.',
    caveat:
      'Profit is not estimated — leg direction is ambiguous in the served rows.',
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
  // Aggregator-derived feed — empty by construction on a network
  // with no aggregator. Nav/search/sitemap no longer offer it there, but
  // a direct URL or an old bookmark still lands here, and an empty table
  // with no explanation reads as an outage.
  // The title stays on this branch: the document carries its <h1> on
  // every network (lib/nav-shell.built.test.ts), and without it the
  // empty state's own heading was the top of the outline.
  if (!routeAvailable('/mev')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'MEV' }]}
          title="MEV"
        />
        <NetworkUnavailable href="/mev" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'MEV' }]}
        title="MEV"
      />

      <MevFeed />

      <Panel
        headingLevel={2}
        title="What we look for"
        hint="Hover a pattern for its definition and caveat"
      >
        <div className="flex flex-wrap gap-2">
          {PATTERNS.map((p) => (
            <Badge
              key={p.name}
              tone="ok"
              dot
              title={`${p.description} Caveat: ${p.caveat}`}
            >
              {p.name}
            </Badge>
          ))}
        </div>
      </Panel>

      <details className="text-ink-muted text-xs">
        <summary className="cursor-pointer">
          Methodology and known limits
        </summary>
        <div className="mt-2 space-y-2">
          <p>
            MEV trades look like ordinary swaps. Detected events get a per-trade
            flag in <code className="font-mono">mev_events</code>; the
            aggregator can exclude flagged trades from VWAP, so the policy lives
            there and the raw observation is kept.
          </p>
          <p>
            Detection is conservative: served trade rows carry no direction, so
            no detector asserts front/back-run intent and{' '}
            <code className="font-mono">profit_usd</code> is always null; each
            event&apos;s <code className="font-mono">detail.note</code> states
            the evidence. Sandwich kinds need the lake&apos;s tx-order index and
            degrade to not-detected (never guessed) when a transaction is not
            indexed. <code className="font-mono">oracle_deviation</code> is
            reserved, and call-tree attribution awaits diagnostic-event capture.
            See the{' '}
            <Link href="/research" className="underline decoration-dotted">
              research index
            </Link>
            .
          </p>
        </div>
      </details>
    </Container>
  );
}
