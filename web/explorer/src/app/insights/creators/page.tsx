import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container } from '@/components/ui';

import { CreatorBoard } from './CreatorBoard';

export const metadata: Metadata = {
  alternates: { canonical: '/insights/creators' },
  title: 'Account creators — who bootstrapped Stellar accounts',
  description:
    'Which accounts brought the most other accounts onto Stellar, what they funded them with, and how much of that created set still exists today.',
};

export default function CreatorsPage() {
  // Route gating (#328): this page inherits the /insights hub's `pricing`
  // capability through network-routes' longest-prefix match. That is the
  // deliberate choice, not an oversight — the board is served by a rollup
  // cycle the lean test nets do not run, so offering the page there would
  // put an empty surface behind a nav link, which is the exact defect the
  // route table was built to stop.
  if (!routeAvailable('/insights/creators')) {
    return (
      <Container className="space-y-6 py-8">
        <h1 className="text-3xl font-semibold tracking-tight">
          Account creators
        </h1>
        <NetworkUnavailable href="/insights/creators" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">
          Account creators
        </h1>
        <p className="text-ink-body max-w-3xl text-sm">
          Every Stellar account was created by another account, which paid its
          starting balance. This board ranks those funders: who onboarded the
          most accounts, what they funded them with, and how much of that
          created set is still around.
        </p>
      </header>

      <CreatorBoard />

      <Panel
        headingLevel={2}
        title="What this measures, and what it does not"
        bodyClassName="text-sm text-ink-body space-y-2"
      >
        <p>
          A row here is the <strong>creator</strong> relationship: the source
          account of a <code className="font-mono text-xs">CreateAccount</code>{' '}
          operation and the account it brought into existence. That link is
          immutable — once an account has been created, nothing later un-creates
          it, so the counts only grow.
        </p>
        <p>
          It is <strong>not</strong> sponsorship. A sponsor pays the base
          reserve for a ledger entry someone else owns, and that arrangement can
          be revoked or handed to another account at any time, so &quot;who
          sponsors what&quot; is a point-in-time question with a different
          answer. Sponsorship is not served by this API yet; nothing on this
          page should be read as a sponsorship figure. The two are tracked
          separately in{' '}
          <Link
            href="https://github.com/Stellar-Index/StellarIndex/issues/351"
            className="underline decoration-dotted"
          >
            issue 351
          </Link>
          .
        </p>
        <p>
          The board is a precomputed rollup, not a live scan, so it is only as
          current as its last cycle — the coverage strip above states the ledger
          span behind the numbers and when they were computed. Read the counts
          as covering that span and nothing wider.
        </p>
      </Panel>
    </Container>
  );
}
