import type { Metadata } from 'next';
import Link from 'next/link';
import { Activity, GitCompare, HandCoins, UserPlus, Zap } from 'lucide-react';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { availableRoutes, routeAvailable } from '@/lib/network-routes';
import { Container } from '@/components/ui';
export const metadata: Metadata = {
  alternates: { canonical: '/insights' },
  title: 'Insights — anomalies, divergence and MEV on Stellar',
  description:
    'The signals layer over the raw data: price-anomaly freezes, cross-venue divergence, and MEV activity observed on the Stellar network.',
};

// The Insights hub (nav revision 2026-08-24): the rail carries ONE entry
// for the signals layer; the three surfaces keep their own routes and
// deep-links. Static hub — each card names the question its surface
// answers, mirroring the /lending hub's shape.
const SURFACES = [
  {
    href: '/anomalies',
    label: 'Anomalies',
    icon: Zap,
    blurb:
      'Price-anomaly freezes: when a pair’s print fails the 3-signal confidence check, publication is withheld and the freeze is recorded here — fire, holds, extensions, release.',
  },
  {
    href: '/divergences',
    label: 'Divergence',
    icon: GitCompare,
    blurb:
      'Cross-venue disagreement: where the on-chain price and external references part ways, by pair and by venue, with the reference chain that flagged it.',
  },
  {
    href: '/mev',
    label: 'MEV',
    icon: Activity,
    blurb:
      'Extractable-value activity observed on Stellar: arbitrage cycles, liquidation races and ordering effects, tied back to the transactions that carried them.',
  },
  {
    href: '/insights/sponsors',
    label: 'Account sponsors',
    icon: HandCoins,
    blurb:
      "Who pays the base reserves for other accounts' ledger entries: sponsorships started, accounts covered and revocations issued, over the whole history of the feature.",
  },
  {
    href: '/insights/creators',
    label: 'Account creators',
    icon: UserPlus,
    blurb:
      'Who bootstrapped the network: the accounts that funded the most other accounts into existence, what they paid to start them, and how much of that created set still exists.',
  },
] as const;

export default function InsightsPage() {
  // #328: the whole signals layer is aggregator-derived. The rail already
  // dropped this hub on the lean test nets; a direct URL still reached it
  // and offered three cards to three empty pages.
  // The title stays on this branch: the document carries its <h1> on
  // every network (lib/nav-shell.built.test.ts), and without it the
  // empty state's own heading was the top of the outline.
  if (!routeAvailable('/insights')) {
    return (
      <Container className="space-y-6 py-8">
        <h1 className="text-3xl font-semibold tracking-tight">Insights</h1>
        <NetworkUnavailable href="/insights" />
      </Container>
    );
  }
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">Insights</h1>
        <p className="text-ink-body max-w-3xl text-sm">
          The signals layer over the raw data — what the network is doing that a
          table of trades doesn’t say by itself.
        </p>
      </header>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {availableRoutes(SURFACES).map((s) => {
          const Icon = s.icon;
          return (
            <Link
              key={s.href}
              href={s.href}
              className="group border-line bg-surface hover:border-brand-500 rounded-xl border p-5 shadow-sm transition-colors"
            >
              <div className="flex items-center gap-2.5">
                <Icon className="text-brand-600 h-5 w-5" />
                <h2 className="group-hover:text-brand-600 text-lg font-semibold tracking-tight">
                  {s.label}
                </h2>
              </div>
              <p className="text-ink-body mt-2 text-sm">{s.blurb}</p>
            </Link>
          );
        })}
      </div>
    </Container>
  );
}
