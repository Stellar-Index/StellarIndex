import Link from 'next/link';

import { EmptyState } from '@/components/ui';
import {
  routeAvailable,
  routeCapability,
  type NetworkCapability,
} from '@/lib/network-routes';
import { CURRENT_NETWORK } from '@/lib/networks';

// Why each capability is missing, in the reader's terms. This is the
// sentence a visitor who typed the URL (or followed an old link) sees
// instead of an empty grid that looks like an outage — #328.
const REASON: Record<NetworkCapability, string> = {
  pricing:
    'This page is built from the price aggregator, which runs on Mainnet only — the test-net deployments index the chain but compute no USD prices, so there is nothing to show here rather than something missing.',
  accounts:
    'API keys and customer accounts live on the Mainnet deployment only; the test-net explorers are free public surfaces with no accounts backend.',
  assets:
    'This network has no issued assets — it is a contracts-only protocol-preview chain, so the asset surfaces are empty by construction.',
  sdex: 'This network has no SDEX order-book activity, so the classic-DEX surfaces are empty by construction.',
  bridges:
    'The cross-chain bridges we index (Circle CCTP, Rozo) are deployed on Mainnet only, so there is no bridge roster on this network.',
};

/**
 * NetworkUnavailable — the honest empty state for a route that has no
 * data on THIS network.
 *
 * Nav, search, the sitemap and the hub card grids all filter these
 * routes out (lib/network-routes), but a route is still reachable by
 * direct URL, by an external link, and from an older bookmark. Without
 * this the reader got an empty table with no explanation, which is
 * indistinguishable from a broken deployment.
 *
 * Renders nothing when the route IS available here, so dropping it at
 * the top of a page body is safe with or without an outer
 * `routeAvailable` guard.
 *
 * That self-suppression is the docstring's long-standing promise; the
 * code did not keep it until 2026-08-31 (wave-D RD-06). It always
 * rendered the EmptyState, and pointed at an `available` helper "below"
 * that was never written. Every current caller happens to guard with
 * `if (!routeAvailable(…))` first, so nothing was visibly broken — but
 * the next network-gated surface written by following this comment
 * (/exchanges and /bridges are both in ROUTE_CAPABILITY with no
 * page-level gate) would have rendered "Not available on Mainnet" above
 * its real content, on mainnet.
 */
export function NetworkUnavailable({ href }: { href: string }) {
  if (routeAvailable(href)) return null;
  const cap = routeCapability(href);
  return (
    <EmptyState
      title={`Not available on ${CURRENT_NETWORK.label}`}
      description={cap ? REASON[cap] : undefined}
      action={
        <Link href="/" className="text-brand-600 text-sm hover:underline">
          Back to the {CURRENT_NETWORK.label} explorer →
        </Link>
      }
    />
  );
}
