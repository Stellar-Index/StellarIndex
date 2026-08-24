import type { Metadata } from 'next';
import Link from 'next/link';
import { Activity, GitCompare, Zap } from 'lucide-react';

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
] as const;

export default function InsightsPage() {
  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">Insights</h1>
        <p className="max-w-3xl text-sm text-ink-body">
          The signals layer over the raw data — what the network is doing
          that a table of trades doesn’t say by itself.
        </p>
      </header>

      <div className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {SURFACES.map((s) => {
          const Icon = s.icon;
          return (
            <Link
              key={s.href}
              href={s.href}
              className="group rounded-xl border border-line bg-surface p-5 shadow-sm transition-colors hover:border-brand-500"
            >
              <div className="flex items-center gap-2.5">
                <Icon className="h-5 w-5 text-brand-600" />
                <h2 className="text-lg font-semibold tracking-tight group-hover:text-brand-600">
                  {s.label}
                </h2>
              </div>
              <p className="mt-2 text-sm text-ink-body">{s.blurb}</p>
            </Link>
          );
        })}
      </div>
    </Container>
  );
}
