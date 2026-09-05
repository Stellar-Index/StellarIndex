import type { Metadata } from 'next';
import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container } from '@/components/ui';

import { SponsorBoard } from './SponsorBoard';

export const metadata: Metadata = {
  alternates: { canonical: '/insights/sponsors' },
  title: 'Account sponsors — who pays Stellar base reserves',
  description:
    "Which accounts have paid the base reserves for other accounts' ledger entries on Stellar, how many accounts they covered, and how many sponsorships they revoked.",
};

export default function SponsorsPage() {
  // Inherits the /insights hub's `pricing` capability by longest-prefix
  // match, for the same reason as the creators board: the lean test nets
  // do not run the rollup cycle, so offering the page there would put an
  // empty surface behind a nav link.
  if (!routeAvailable('/insights/sponsors')) {
    return (
      <Container className="space-y-6 py-8">
        <h1 className="text-3xl font-semibold tracking-tight">
          Account sponsors
        </h1>
        <NetworkUnavailable href="/insights/sponsors" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <header className="space-y-2">
        <h1 className="text-3xl font-semibold tracking-tight">
          Account sponsors
        </h1>
        <p className="text-ink-body max-w-3xl text-sm">
          Holding anything on Stellar costs a base reserve, and one account can
          pay that reserve on another&apos;s behalf. This board ranks the
          accounts doing the paying: how many sponsorship arrangements they have
          started, how many distinct accounts those covered, and how many they
          have revoked.
        </p>
      </header>

      <SponsorBoard />

      <Panel
        headingLevel={2}
        title="This is history, not a live sponsor list"
        bodyClassName="text-sm text-ink-body space-y-2"
      >
        <p>
          Every number here is derived by replaying sponsorship{' '}
          <strong>operations</strong> — the begin/end pair that opens an
          arrangement, and the revocations that end one. That tells you exactly
          what an account has done, and it is complete: the board covers every
          sponsorship operation since protocol 14 introduced the feature.
        </p>
        <p>
          What it deliberately does <strong>not</strong> tell you is who is
          sponsoring what <em>right now</em>. A sponsorship also lapses without
          any operation at all — when the sponsored entry is simply deleted, or
          the sponsored account merges away. So a &quot;currently
          sponsoring&quot; count built from this source would be too high, and
          none is shown. Answering that question needs the sponsor recorded on
          each live ledger entry, which is a separate reader this API does not
          expose yet.
        </p>
        <p>
          For the same reason <strong>revocations issued</strong> is a lower
          bound on arrangements that ended, not a total. And sponsorship is not
          account creation: creating an account is immutable and happens once,
          sponsoring is revocable and repeatable, so the{' '}
          <Link
            href="/insights/creators"
            className="underline decoration-dotted"
          >
            creators board
          </Link>{' '}
          answers a different question and the two should not be added together.
          An account can rank highly on both.
        </p>
      </Panel>
    </Container>
  );
}
