import type { Metadata } from 'next';
import Link from 'next/link';

import { DivergenceFeed } from './DivergenceFeed';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Badge, Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/divergences' },
  title: 'Divergences — cross-reference monitor',
  description:
    'Continuously cross-checks the canonical Stellar Index VWAP against external references (CoinGecko plus the on-chain Reflector, Redstone, Band and Chainlink oracles). Persistent gaps flip flags.divergence_warning.',
};

// status reflects what the divergence WORKER actually cross-checks
// today — not just feeds we ingest elsewhere:
//   active     — producing divergence_observations rows now
//                (Reflector/Redstone/Band went live 2026-07 as
//                on-chain references: the worker compares the
//                latest ingested oracle_updates value against our
//                VWAP for pairs both sides cover)
//   configured — implemented + operator-configured, may be between
//                refreshes / awaiting upstream data
//   planned    — described, but not yet wired as a divergence check
type RefStatus = 'active' | 'configured' | 'planned';
type Reference = {
  name: string;
  type: string;
  blurb: string;
  status: RefStatus;
};
const REFERENCES: Reference[] = [
  {
    name: 'CoinGecko',
    type: 'HTTP price index',
    status: 'active',
    blurb:
      "Aggregator-of-aggregators. Useful as a sanity reference because it's not on-chain and pulls from a different upstream set.",
  },
  {
    name: 'Chainlink',
    type: 'On-chain data feeds',
    status: 'active',
    blurb:
      'Chainlink Data Feeds published on Stellar, compared from our ingested oracle_updates rows. Feeds update on a price deviation or a 24-hour heartbeat, so a reading up to 26 hours old still counts.',
  },
  {
    name: 'Reflector (DEX/CEX/FX)',
    type: 'On-chain SEP-40 oracle',
    status: 'active',
    blurb:
      'Stellar-native oracle trio, compared as three references (reflector-dex/cex/fx) from our own ingested oracle_updates rows. Reflector divergence often signals an oracle update lag rather than a real price move — important to distinguish for downstream consumers like Blend.',
  },
  {
    name: 'Redstone',
    type: 'On-chain adapter contract',
    status: 'active',
    blurb:
      'Pull-style oracle on Stellar, compared from our ingested oracle_updates rows. Divergence here is rare but high-signal — Redstone batches many feeds in one transaction so divergence on one feed often precedes a wider reading update.',
  },
  {
    name: 'Band',
    type: 'On-chain Soroban contract (no events)',
    status: 'active',
    blurb:
      'Operation-args ingest (Band emits zero events). The divergence check reads the same relayed value the on-chain consumer would see, straight from our ingested rows.',
  },
];

const REF_STATUS: Record<
  RefStatus,
  { label: string; tone: 'ok' | 'warn' | 'neutral' }
> = {
  active: { label: 'Active', tone: 'ok' },
  configured: { label: 'Configured', tone: 'warn' },
  planned: { label: 'Planned', tone: 'neutral' },
};

export default function DivergencesPage() {
  // Aggregator-derived feed — empty by construction on a network
  // with no aggregator. Nav/search/sitemap no longer offer it there, but
  // a direct URL or an old bookmark still lands here, and an empty table
  // with no explanation reads as an outage.
  // The title stays on this branch: the document carries its <h1> on
  // every network (lib/nav-shell.built.test.ts), and without it the
  // empty state's own heading was the top of the outline.
  if (!routeAvailable('/divergences')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Divergences' }]}
          title="Divergences"
        />
        <NetworkUnavailable href="/divergences" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Divergences' }]}
        title="Divergences"
      />

      <DivergenceFeed />

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
        {REFERENCES.map((r) => (
          <div
            key={r.name}
            className="rounded-card border-line bg-surface border p-4"
          >
            <div className="flex items-center gap-2">
              <h2 className="text-h3 text-ink font-semibold" title={r.blurb}>
                {r.name}
              </h2>
              <Badge tone={REF_STATUS[r.status].tone}>
                {REF_STATUS[r.status].label}
              </Badge>
            </div>
            <p className="text-ink-muted mt-1 text-xs">{r.type}</p>
          </div>
        ))}
      </div>

      <p className="text-ink-muted text-xs">
        Δ% ={' '}
        <code className="font-mono">(our − reference) / reference × 100</code>.
        References never enter our VWAP; a breach sets{' '}
        <code className="font-mono">flags.divergence_warning</code> (
        <Link href="/research/adr/0019" className="underline decoration-dotted">
          ADR-0019
        </Link>
        ).
      </p>
    </Container>
  );
}
