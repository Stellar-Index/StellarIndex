// Per-network resolution of the outbound-explorer helpers.
//
// The hardcode guard (network-hardcodes.test.ts) proves no MAINNET literal
// survives in source. It cannot prove the replacements RESOLVE correctly —
// a helper that returned mainnet URLs on every network would pass it. These
// tests pin the actual values per network, including the one case that has
// no correct answer: stellar.expert does not host futurenet, so the helper
// must return null there and callers must render no link rather than one
// pointing at another chain.
//
// CURRENT_NETWORK_ID is resolved from process.env.NEXT_PUBLIC_NETWORK at
// MODULE LOAD, so each case stubs the env and re-imports with a fresh
// module registry.
import { afterEach, describe, expect, it, vi } from 'vitest';

async function loadFor(network: string | undefined) {
  vi.resetModules();
  if (network === undefined) vi.stubEnv('NEXT_PUBLIC_NETWORK', '');
  else vi.stubEnv('NEXT_PUBLIC_NETWORK', network);
  return import('./networks');
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

const TX = 'abc123';
const ACC = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

describe('stellarExpertUrl', () => {
  it('uses the /public segment on mainnet', async () => {
    const { stellarExpertUrl } = await loadFor('mainnet');
    expect(stellarExpertUrl('tx', TX)).toBe(
      `https://stellar.expert/explorer/public/tx/${TX}`,
    );
  });

  it('uses the /testnet segment on testnet', async () => {
    const { stellarExpertUrl } = await loadFor('testnet');
    expect(stellarExpertUrl('account', ACC)).toBe(
      `https://stellar.expert/explorer/testnet/account/${ACC}`,
    );
  });

  it('returns null on futurenet — stellar.expert has no explorer for it', async () => {
    // The whole point: a link to another network's explorer is worse than no
    // link, because the id may resolve there to an unrelated entity.
    const { stellarExpertUrl } = await loadFor('futurenet');
    expect(stellarExpertUrl('tx', TX)).toBeNull();
    expect(stellarExpertUrl('account', ACC)).toBeNull();
  });

  it('falls back to mainnet when the network env is unset', async () => {
    const { stellarExpertUrl, CURRENT_NETWORK_ID } = await loadFor(undefined);
    expect(CURRENT_NETWORK_ID).toBe('mainnet');
    expect(stellarExpertUrl('tx', TX)).toContain('/explorer/public/');
  });
});

describe('stellarChainEntityUrl', () => {
  // stellarchain.io hosts all three networks, which is why it is the one
  // cross-reference that still works on futurenet.
  it.each([
    ['mainnet', 'https://stellarchain.io'],
    ['testnet', 'https://testnet.stellarchain.io'],
    ['futurenet', 'https://futurenet.stellarchain.io'],
  ])('uses the %s origin', async (network, origin) => {
    const { stellarChainEntityUrl } = await loadFor(network);
    expect(stellarChainEntityUrl('transactions', TX)).toBe(
      `${origin}/transactions/${TX}`,
    );
  });

  it('uses the PLURAL path segments the site actually serves', async () => {
    // Verified live 2026-08-27 by response size: /transactions/, /accounts/
    // and /contracts/ return a server-rendered page while the singular forms
    // return the same empty SPA shell as a nonsense path. Status code cannot
    // tell them apart — the SPA answers 200 for everything.
    const { stellarChainEntityUrl } = await loadFor('mainnet');
    expect(stellarChainEntityUrl('accounts', ACC)).toContain('/accounts/');
    expect(stellarChainEntityUrl('contracts', 'C123')).toContain('/contracts/');
  });
});

describe('explorer + API origins', () => {
  it.each([
    ['mainnet', 'https://stellarindex.io', 'https://api.stellarindex.io'],
    [
      'testnet',
      'https://testnet.stellarindex.io',
      'https://api.testnet.stellarindex.io',
    ],
    [
      'futurenet',
      'https://futurenet.stellarindex.io',
      'https://api.futurenet.stellarindex.io',
    ],
  ])('resolves %s to its own origins', async (network, explorer, api) => {
    // These back canonicals, sitemap/robots and API_BASE_URL's fallback. A
    // test-net build resolving to the mainnet origin is what made the test
    // nets advertise themselves as production.
    const { CURRENT_NETWORK } = await loadFor(network);
    expect(CURRENT_NETWORK.explorerUrl).toBe(explorer);
    expect(CURRENT_NETWORK.apiBaseUrl).toBe(api);
  });

  it('names the network the way Stellar does, not the way our switcher does', async () => {
    // stellarName vs label: mainnet is "Pubnet" to Stellar and "Mainnet" in
    // our UI. Conflating them is what put "Pubnet" on the testnet page.
    const main = await loadFor('mainnet');
    expect(main.CURRENT_NETWORK.stellarName).toBe('Pubnet');
    expect(main.CURRENT_NETWORK.label).toBe('Mainnet');
    const test = await loadFor('testnet');
    expect(test.CURRENT_NETWORK.stellarName).toBe('Testnet');
  });
});
