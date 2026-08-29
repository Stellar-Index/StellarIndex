// The build-time env contract between next.config.mjs and src/api/client.ts.
//
// client.ts resolves `API_BASE_URL = process.env.NEXT_PUBLIC_API_BASE_URL ??
// CURRENT_NETWORK.apiBaseUrl` — the fallback is THIS network's own API
// origin. Next inlines `process.env.NEXT_PUBLIC_*` at build time; anything
// placed in next.config's `env` block is inlined too, and a default there
// wins over the fallback in client.ts because the `??` in client.ts sees a
// string, never `undefined`.
//
// That is exactly how a testnet/futurenet build with NEXT_PUBLIC_NETWORK set
// but NEXT_PUBLIC_API_BASE_URL forgotten (the git-integrated CF Pages
// projects are hand-configured; see docs/operations/explorer-deployment.md)
// used to serve MAINNET data under test-net chrome: next.config.mjs inlined
// 'https://api.stellarindex.io' whenever the var was unset, so client.ts's
// network-aware fallback was dead code.
//
// These tests load next.config.mjs and client.ts under a stubbed env the
// way `next build` would see it and pin the resolution end to end.
import { afterEach, describe, expect, it, vi } from 'vitest';

async function loadConfigFor(env: Record<string, string | undefined>) {
  vi.resetModules();
  for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
  // next.config.mjs is plain JS (tsconfig has allowJs: false) so it carries
  // no declaration; the shape we depend on is asserted right here.
  // @ts-expect-error TS7016 — untyped .mjs module
  const mod = (await import('../../next.config.mjs')) as {
    default: { env?: Record<string, string | undefined> };
  };
  return mod.default;
}

async function loadClientFor(env: Record<string, string | undefined>) {
  vi.resetModules();
  for (const [k, v] of Object.entries(env)) vi.stubEnv(k, v);
  return import('@/api/client');
}

afterEach(() => {
  vi.unstubAllEnvs();
  vi.resetModules();
});

describe('next.config.mjs env block', () => {
  it.each(['testnet', 'futurenet', 'mainnet', undefined])(
    'inlines NO NEXT_PUBLIC_API_BASE_URL default when the var is unset (network=%s)',
    async (network) => {
      const cfg = await loadConfigFor({
        NEXT_PUBLIC_NETWORK: network,
        NEXT_PUBLIC_API_BASE_URL: undefined,
      });
      // The key must be ABSENT, not merely undefined-valued: Next validates
      // `env` values as strings, and any string here would shadow the
      // per-network fallback in client.ts for every build that forgot the
      // var — which is the git-integrated test-net projects.
      expect(Object.keys(cfg.env ?? {})).not.toContain(
        'NEXT_PUBLIC_API_BASE_URL',
      );
    },
  );

  it('never inlines the mainnet API origin from the config itself', async () => {
    const cfg = await loadConfigFor({ NEXT_PUBLIC_API_BASE_URL: undefined });
    expect(Object.values(cfg.env ?? {})).not.toContain(
      'https://api.stellarindex.io',
    );
  });
});

describe('API_BASE_URL resolution (client.ts)', () => {
  it.each([
    ['testnet', 'https://api.testnet.stellarindex.io'],
    ['futurenet', 'https://api.futurenet.stellarindex.io'],
    ['mainnet', 'https://api.stellarindex.io'],
  ])(
    'falls back to the %s API origin when NEXT_PUBLIC_API_BASE_URL is unset',
    async (network, api) => {
      const { API_BASE_URL } = await loadClientFor({
        NEXT_PUBLIC_NETWORK: network,
        NEXT_PUBLIC_API_BASE_URL: undefined,
      });
      expect(API_BASE_URL).toBe(api);
    },
  );

  it('honours an explicit NEXT_PUBLIC_API_BASE_URL (the CI stub / break-glass path)', async () => {
    const { API_BASE_URL } = await loadClientFor({
      NEXT_PUBLIC_NETWORK: 'testnet',
      NEXT_PUBLIC_API_BASE_URL: 'http://api.ci-stub.invalid',
    });
    expect(API_BASE_URL).toBe('http://api.ci-stub.invalid');
  });
});
