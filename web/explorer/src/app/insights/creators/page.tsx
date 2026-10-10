import type { Metadata } from 'next';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container, PageHeader } from '@/components/ui';

import { CreatorBoard } from './CreatorBoard';

export const metadata: Metadata = {
  alternates: { canonical: '/insights/creators' },
  title: 'Account creators — who bootstrapped Stellar accounts',
  description:
    'Which accounts brought the most other accounts onto Stellar, what they funded them with, and how much of that created set still exists today.',
};

const CRUMBS = [
  { label: 'Home', href: '/' },
  { label: 'Insights', href: '/insights' },
  { label: 'Account creators' },
];

export default function CreatorsPage() {
  // Route gating: this page inherits the /insights hub's `pricing`
  // capability through network-routes' longest-prefix match. That is the
  // deliberate choice, not an oversight — the board is served by a rollup
  // cycle the lean test nets do not run, so offering the page there would
  // put an empty surface behind a nav link, which is the exact defect the
  // route table was built to stop.
  if (!routeAvailable('/insights/creators')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader title="Account creators" breadcrumbs={CRUMBS} />
        <NetworkUnavailable href="/insights/creators" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader title="Account creators" breadcrumbs={CRUMBS} />

      <CreatorBoard />

      <details className="text-ink-muted text-xs">
        <summary className="cursor-pointer">
          What this measures, and what it does not
        </summary>
        <div className="mt-2 max-w-3xl space-y-2">
          <p>
            A row is the <strong>creator</strong> relationship: the source of a{' '}
            <code className="font-mono">CreateAccount</code> operation and the
            account it created. That link is immutable, so counts only grow.
          </p>
          <p>
            It is <strong>not</strong> sponsorship, which is revocable and
            point-in-time and is not served here; nothing on this page is a
            sponsorship figure.
          </p>
          <p>
            The board is a precomputed rollup, only as current as its last
            cycle. Read the counts as covering the stated ledger span and
            nothing wider.
          </p>
        </div>
      </details>
    </Container>
  );
}
