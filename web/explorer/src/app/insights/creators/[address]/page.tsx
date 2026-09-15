import type { Metadata } from 'next';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container } from '@/components/ui';

import { CreatorDetailPathView } from './CreatorDetailPathView';

/**
 * /insights/creators/{g_strkey} — one account creator in detail.
 *
 * `/creator/{g_strkey}` is the short alias a reader is most likely to
 * type or be handed; public/_redirects 301s it here, the same way
 * /address/{g} has always 301'd to /accounts/{g}. The canonical URL is
 * this one, under the board it belongs to.
 *
 * ONE SHELL, NOT 955,023 PAGES. See the sibling sponsor route for the
 * full rationale — the creator population is three orders of magnitude
 * past anything generateStaticParams can enumerate, so the build emits
 * one `shell` document and functions/insights/creators/[[path]].js
 * serves it for any address.
 *
 * NOT indexable: the built document is an empty shell, and a crawler
 * that fetched one would be offered a soft-404 under every address it
 * tried. `follow: true` — the links out of it are real.
 *
 * No <Suspense> around the view. Under output:'export' a boundary that
 * is pending at build time bakes a dead subtree into the shell; only a
 * useSearchParams child needs one, and this view reads the PATH.
 */
export const dynamicParams = false;

export function generateStaticParams() {
  return [{ address: 'shell' }];
}

export const metadata: Metadata = {
  title: 'Creator detail — accounts created over time',
  description:
    'Every account this address has created, the monthly history of its funding activity, its standing on the creator board, and the assets it holds.',
  robots: { index: false, follow: true },
};

export default function CreatorDetailPage() {
  // Inherits the /insights hub's `pricing` capability by longest-prefix
  // match, exactly as the board above it does: the lean test nets do not
  // run the rollup cycle behind this graph, so the page would be empty
  // by construction rather than merely quiet.
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
  return <CreatorDetailPathView />;
}
