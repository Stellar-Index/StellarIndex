// Regression suite for wave-D RD-06: NetworkUnavailable's JSDoc
// promised it "renders nothing when the route IS available here" — and
// pointed at an `available` helper "below" that was never written —
// while the component unconditionally rendered the EmptyState.
//
// Nothing was visibly broken, because all five callers happen to guard
// with `if (!routeAvailable(…))` first. The cost was the trap left for
// the next one: /exchanges and /bridges are both in ROUTE_CAPABILITY
// with no page-level gate, so an author following the docstring would
// drop <NetworkUnavailable href="/exchanges" /> at the top of the body,
// trust it to self-suppress, and ship "Not available on Mainnet" above
// the real content — on mainnet.
//
// The available-route branch is the case no existing test covered,
// which is exactly why the docstring could drift from the code.
//
// CURRENT_NETWORK resolves from process.env.NEXT_PUBLIC_NETWORK at
// MODULE LOAD, so each case re-imports with a fresh module registry —
// same convention as network-routes.test.ts.
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

async function loadFor(network: string) {
  vi.resetModules();
  vi.stubEnv('NEXT_PUBLIC_NETWORK', network);
  return import('./NetworkUnavailable');
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe('NetworkUnavailable', () => {
  it('renders nothing when the route IS available on this network', async () => {
    // /markets is pricing-gated and mainnet HAS the aggregator.
    const { NetworkUnavailable } = await loadFor('pubnet');
    const { container } = render(<NetworkUnavailable href="/markets" />);
    expect(container).toBeEmptyDOMElement();
  });

  it('renders the honest empty state when the route is NOT available', async () => {
    // Testnet runs no price aggregator, so /markets is gated off.
    const { NetworkUnavailable } = await loadFor('testnet');
    render(<NetworkUnavailable href="/markets" />);
    expect(screen.getByText(/Not available on/i)).toBeInTheDocument();
    // The reason line is the point of the component — an unexplained
    // empty grid is indistinguishable from an outage.
    expect(screen.getByText(/price aggregator/i)).toBeInTheDocument();
  });

  it('renders for an ungated route only when that route is unavailable', async () => {
    // A route with no capability entry is available everywhere, so the
    // component must stay silent rather than claim otherwise.
    const { NetworkUnavailable } = await loadFor('testnet');
    const { container } = render(<NetworkUnavailable href="/ledgers" />);
    expect(container).toBeEmptyDOMElement();
  });
});
