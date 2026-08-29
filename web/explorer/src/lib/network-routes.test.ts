// Per-network route availability — the contract every nav, search,
// sitemap and card-grid surface now asks instead of keeping its own copy.
//
// #328: /anomalies, /divergences and /mev are aggregator-derived and empty
// on every net without an aggregator. They were dropped from the rail (via
// their /insights hub) but still listed by the sitemap and still carded on
// the /network hub, because each surface carried its own hidden-href set.
// This file pins the table those surfaces read, per network.
//
// CURRENT_NETWORK_ID resolves from process.env.NEXT_PUBLIC_NETWORK at
// MODULE LOAD, so each case re-imports with a fresh module registry —
// same convention as networks.test.ts.
import { afterEach, describe, expect, it, vi } from 'vitest';

async function loadFor(network: string) {
  vi.resetModules();
  vi.stubEnv('NEXT_PUBLIC_NETWORK', network);
  return import('./network-routes');
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

// The three feeds the issue was filed about, plus the rest of the
// aggregator surface they belong to.
const PRICING_SURFACE = [
  '/anomalies',
  '/divergences',
  '/mev',
  '/insights',
  '/markets',
  '/exchanges',
  '/lending',
  '/aggregators',
  '/oracles',
  '/amm',
  '/yield',
  '/convert/USD/EUR',
  '/external/assets',
];

// Chain-native surfaces: real data on every network, never gated.
const ALWAYS_ON = [
  '/',
  '/ledgers',
  '/transactions',
  '/accounts',
  '/contracts',
  '/network',
  '/protocols',
  '/sources',
  '/status',
  '/diagnostics',
  '/docs',
  '/methodology',
];

describe('routeAvailable', () => {
  it.each(PRICING_SURFACE)('offers %s on mainnet', async (href) => {
    const { routeAvailable } = await loadFor('mainnet');
    expect(routeAvailable(href)).toBe(true);
  });

  it.each(PRICING_SURFACE)('withholds %s on testnet', async (href) => {
    const { routeAvailable } = await loadFor('testnet');
    expect(routeAvailable(href)).toBe(false);
  });

  it.each(PRICING_SURFACE)('withholds %s on futurenet', async (href) => {
    const { routeAvailable } = await loadFor('futurenet');
    expect(routeAvailable(href)).toBe(false);
  });

  it.each(ALWAYS_ON)('keeps %s on every network', async (href) => {
    for (const net of ['mainnet', 'testnet', 'futurenet']) {
      const { routeAvailable } = await loadFor(net);
      expect(routeAvailable(href)).toBe(true);
    }
  });

  it('gates the asset + SDEX surfaces on futurenet only', async () => {
    // Futurenet is contracts-only (0 assets, 0 SDEX trades); testnet has
    // both. Before #328 this fact lived as an `id === 'futurenet'` check on
    // the homepage alone, so futurenet's nav still offered both.
    const futurenet = await loadFor('futurenet');
    for (const href of ['/assets', '/issuers', '/sdex', '/liquidity-pools']) {
      expect(futurenet.routeAvailable(href)).toBe(false);
    }
    const testnet = await loadFor('testnet');
    for (const href of ['/assets', '/issuers', '/sdex', '/liquidity-pools']) {
      expect(testnet.routeAvailable(href)).toBe(true);
    }
  });

  it('gates the accounts SaaS surface off both test nets', async () => {
    for (const net of ['testnet', 'futurenet']) {
      const { routeAvailable } = await loadFor(net);
      for (const href of ['/signin', '/signup', '/dashboard']) {
        expect(routeAvailable(href)).toBe(false);
      }
    }
  });

  it('gates /bridges off both test nets — CCTP + Rozo are pubnet-only', async () => {
    expect((await loadFor('mainnet')).routeAvailable('/bridges')).toBe(true);
    expect((await loadFor('testnet')).routeAvailable('/bridges')).toBe(false);
  });

  it('inherits the hub rule for nested routes', async () => {
    // A sub-route must not slip past because the table only names its hub —
    // that is exactly how a per-component hidden-href SET behaves, and why
    // this is prefix-matched instead.
    const { routeAvailable } = await loadFor('testnet');
    expect(routeAvailable('/markets/XLM-USDC')).toBe(false);
    expect(routeAvailable('/exchanges/kraken')).toBe(false);
    expect(routeAvailable('/convert/USD/EUR')).toBe(false);
    expect(routeAvailable('/ledgers/12345')).toBe(true);
  });

  it('matches the LONGEST declared prefix, not the first', async () => {
    // '/external/assets' is pricing-gated while '/assets' is asset-gated;
    // on futurenet both are off, but on testnet only the former is — which
    // only holds if the longer key wins.
    const { routeAvailable } = await loadFor('testnet');
    expect(routeAvailable('/external/assets')).toBe(false);
    expect(routeAvailable('/assets')).toBe(true);
  });

  it('never gates an off-site link', async () => {
    const { routeAvailable } = await loadFor('futurenet');
    expect(routeAvailable('https://docs.stellarindex.io')).toBe(true);
  });

  it('ignores query strings and fragments', async () => {
    const { routeAvailable } = await loadFor('testnet');
    expect(routeAvailable('/markets?asset=native')).toBe(false);
    expect(routeAvailable('/ledgers?cursor=1#top')).toBe(true);
  });
});

describe('availableRoutes', () => {
  it('filters a link list down to what this network has', async () => {
    const { availableRoutes } = await loadFor('testnet');
    const links = [
      { href: '/ledgers', label: 'Ledgers' },
      { href: '/mev', label: 'MEV' },
      { href: '/anomalies', label: 'Anomalies' },
      { href: '/divergences', label: 'Divergences' },
      { href: '/assets', label: 'Assets' },
    ];
    expect(availableRoutes(links).map((l) => l.href)).toEqual([
      '/ledgers',
      '/assets',
    ]);
  });
});
