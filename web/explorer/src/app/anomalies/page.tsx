import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';

import { AnomaliesFeed } from './AnomaliesFeed';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container, PageHeader } from '@/components/ui';
export const metadata: Metadata = {
  title: 'Anomalies — freeze and outlier timeline',
  description:
    'Every clear→firing freeze transition, with reason + recovery + frozen-value detail.',
  alternates: { canonical: '/anomalies' },
};

// Only these three are ever written by the automated freeze mapper
// (internal/storage/timescale/freeze_events.go's mapFreezeReason).
// `single_source` and `manual` are reserved reason-CHECK values with
// no writer yet — single-source deviations currently fold into
// `outlier_storm`, and `manual` awaits a genuinely operator-initiated
// freeze path — so they are deliberately omitted here rather than
// listed as things a reader could see fire.
const REASONS: { name: string; trigger: string; meaning: string }[] = [
  {
    name: 'divergence',
    trigger: 'Persistent gap vs an external reference',
    meaning:
      'Our VWAP and an authority reference (CoinGecko, Chainlink HTTP, or a Reflector feed) have been diverging beyond threshold for too long. Almost always means a decoder bug or a stuck source.',
  },
  {
    name: 'outlier_storm',
    trigger: 'Low confidence, high z-score, and a thin source count together',
    meaning:
      'Confidence, z-score and source count all crossed threshold at once; a freeze needs all three, not any one alone. Usually a ledger-level shock; the freeze prevents the surviving inliers from setting a misleading "VWAP".',
  },
  {
    name: 'other',
    trigger: 'Unclassified automated freeze',
    meaning:
      'An automated freeze whose decision shape the recorder did not recognize. Should be rare.',
  },
];

export default function AnomaliesPage() {
  // Aggregator-derived feed — empty by construction on a network
  // with no aggregator. Nav/search/sitemap no longer offer it there, but
  // a direct URL or an old bookmark still lands here, and an empty table
  // with no explanation reads as an outage.
  // The title stays on this branch: the document carries its <h1> on
  // every network (lib/nav-shell.built.test.ts), and without it the
  // empty state's own heading was the top of the outline.
  if (!routeAvailable('/anomalies')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader
          breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Anomalies' }]}
          title="Anomalies"
        />
        <NetworkUnavailable href="/anomalies" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader
        breadcrumbs={[{ label: 'Home', href: '/' }, { label: 'Anomalies' }]}
        title="Anomalies"
        description={
          <>
            Every clear→firing freeze, with reason, recovery and the frozen
            value still served via{' '}
            <code className="font-mono text-xs">/v1/price</code>.
          </>
        }
      />

      <AnomaliesFeed />

      <Panel
        headingLevel={2}
        title="What freezes a pair"
        hint="Hover a reason code for its trigger and meaning"
      >
        <div className="flex flex-wrap gap-2">
          {REASONS.map((r) => (
            <code
              key={r.name}
              className="text-down-strong bg-surface-muted cursor-help rounded-sm px-1.5 py-0.5 font-mono text-[11px]"
              title={`${r.trigger}: ${r.meaning}`}
            >
              {r.name}
            </code>
          ))}
        </div>
        <details className="text-ink-muted mt-3 text-xs">
          <summary className="cursor-pointer">Freeze rules</summary>
          <p className="mt-1">
            Per{' '}
            <Link
              href="/research/adr/0019"
              className="underline decoration-dotted"
            >
              ADR-0019
            </Link>
            , a freeze fires only when confidence, z-score and source count all
            cross threshold together;{' '}
            <code className="font-mono">divergence</code> is the separate
            multi-source-disagreement path. While frozen the API serves the last
            good value with <code className="font-mono">flags.frozen=true</code>
            . A freeze holds 10 or 30 minutes, extends up to four times, then
            escalates and stays firing until an operator runs{' '}
            <code className="font-mono">freeze-unfreeze</code>.
          </p>
        </details>
      </Panel>
    </Container>
  );
}
