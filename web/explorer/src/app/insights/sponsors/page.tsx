import type { Metadata } from 'next';
import Link from 'next/link';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Badge, Container, PageHeader } from '@/components/ui';

import { SponsorBoard } from './SponsorBoard';

export const metadata: Metadata = {
  alternates: { canonical: '/insights/sponsors' },
  title: 'Account sponsors — who pays Stellar base reserves',
  description:
    "Which accounts have paid the base reserves for other accounts' ledger entries on Stellar, how many accounts they covered, and how many sponsorships they revoked.",
};

const CRUMBS = [
  { label: 'Home', href: '/' },
  { label: 'Insights', href: '/insights' },
  { label: 'Account sponsors' },
];

const NOTES = [
  {
    label: 'History, not live',
    tip: 'Replayed from sponsorship operations. A sponsorship also lapses when the entry is deleted or the account merges, with no operation, so no "currently sponsoring" count is shown.',
  },
  {
    label: 'Effective ops only',
    tip: 'Operations in failed transactions are excluded: about 1 in 9 sponsorship operations sit in a failed transaction.',
  },
  {
    label: 'Revocations: lower bound',
    tip: 'Revocations issued undercounts arrangements that ended, since lapses leave no operation.',
  },
];

export default function SponsorsPage() {
  // Inherits the /insights hub's `pricing` capability by longest-prefix
  // match, for the same reason as the creators board: the lean test nets
  // do not run the rollup cycle, so offering the page there would put an
  // empty surface behind a nav link.
  if (!routeAvailable('/insights/sponsors')) {
    return (
      <Container className="space-y-6 py-8">
        <PageHeader title="Account sponsors" breadcrumbs={CRUMBS} />
        <NetworkUnavailable href="/insights/sponsors" />
      </Container>
    );
  }

  return (
    <Container className="space-y-6 py-8">
      <PageHeader title="Account sponsors" breadcrumbs={CRUMBS} />

      <SponsorBoard />

      <div className="flex flex-wrap items-center gap-2 text-xs">
        {NOTES.map((n) => (
          <Badge key={n.label} tone="neutral" title={n.tip}>
            {n.label}
          </Badge>
        ))}
        <Link
          href="/insights/creators"
          className="text-ink-muted underline decoration-dotted"
          title="Account creation is one-off and immutable; sponsorship is revocable. Don't add the two boards together."
        >
          Not account creation → creators board
        </Link>
      </div>
    </Container>
  );
}
