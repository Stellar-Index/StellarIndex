import type { Metadata } from 'next';

import { NetworkUnavailable } from '@/components/NetworkUnavailable';
import { routeAvailable } from '@/lib/network-routes';
import { Container } from '@/components/ui';

import { SponsorDetailPathView } from './SponsorDetailPathView';

/**
 * /insights/sponsors/{g_strkey} — one sponsor in detail.
 *
 * `/sponsor/{g_strkey}` is the short alias a reader is most likely to
 * type or be handed; public/_redirects 301s it here, the same way
 * /address/{g} has always 301'd to /accounts/{g}. The canonical URL is
 * this one, under the board it belongs to.
 *
 * ONE SHELL, NOT 2,427 PAGES. This is a static export. Pre-rendering a
 * page per sponsor would mean 2,427 build-time round trips for a set
 * that grows on its own, and the sibling creator route's population is
 * 955,023 — so both use the shell pattern that /accounts/[g],
 * /issuers/[g_strkey] and /contracts/[id] already use: build one
 * `shell` document, let functions/insights/sponsors/[[path]].js serve it
 * for any address, and read the address from the URL at runtime.
 *
 * NOT indexable, for the same reason /accounts/[g] is not: the built
 * document is an empty shell, and a crawler that fetched one would be
 * offered a soft-404 under every address it tried. `follow: true` — the
 * links out of it are real.
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
  title: 'Sponsor detail — accounts sponsored over time',
  description:
    'Every account this address has sponsored, the monthly history of its sponsorship activity, its standing on the sponsor board, and the assets it holds.',
  robots: { index: false, follow: true },
};

export default function SponsorDetailPage() {
  // Inherits the /insights hub's `pricing` capability by longest-prefix
  // match, exactly as the board above it does: the lean test nets do not
  // run the rollup cycle behind this graph, so the page would be empty
  // by construction rather than merely quiet.
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
  return <SponsorDetailPathView />;
}
