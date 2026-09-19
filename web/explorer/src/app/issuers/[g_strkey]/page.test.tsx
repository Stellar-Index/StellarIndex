import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

vi.mock('@/lib/buildFetch', async () => {
  const actual =
    await vi.importActual<typeof import('@/lib/buildFetch')>(
      '@/lib/buildFetch',
    );
  return { ...actual, buildFetchData: vi.fn(), failBuild: vi.fn() };
});

import { buildFetchData } from '@/lib/buildFetch';
import IssuerDetailPage, { generateMetadata } from './page';
import { CURRENT_NETWORK } from '@/lib/networks';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function mockIssuer(detail: Record<string, unknown>) {
  vi.mocked(buildFetchData).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/issuers/')) return detail as never;
    return [] as never; // /v1/assets?issuer= price join
  });
}

async function renderPage() {
  render(await IssuerDetailPage({ params: Promise.resolve({ g_strkey: G }) }));
}

// Frontend-honesty sweep: /v1/issuers/{g} soft-fails its per-asset
// fan-out on error OR deadline (internal/api/v1/issuers.go), so `assets`
// arrives absent. `?? 0` baked "Assets 0" / "Total observations 0" /
// "Issued assets (0)" into the static export — three headline claims
// that the issuer has issued nothing and never traded.
describe('IssuerDetailPage issued-asset tiles', () => {
  it('renders — for the asset tiles when `assets` is absent', async () => {
    mockIssuer({ g_strkey: G, home_domain: 'circle.com' });
    await renderPage();

    expect(screen.getByText('Assets').nextElementSibling?.textContent).toBe(
      '—',
    );
    expect(
      screen.getByText('Total observations').nextElementSibling?.textContent,
    ).toBe('—');
    // Panel heading carries no fabricated count, and the body abstains.
    expect(screen.getByText('Issued assets')).toBeInTheDocument();
    expect(
      screen.getByText(/Issued-asset list unavailable/),
    ).toBeInTheDocument();
    expect(
      screen.queryByText(/No issued assets observed/),
    ).not.toBeInTheDocument();
  });

  it('renders real counts (including a genuine 0) when `assets` is served', async () => {
    mockIssuer({ g_strkey: G, home_domain: 'circle.com', assets: [] });
    await renderPage();

    expect(screen.getByText('Assets').nextElementSibling?.textContent).toBe(
      '0',
    );
    expect(
      screen.getByText('Total observations').nextElementSibling?.textContent,
    ).toBe('0');
    expect(screen.getByText('Issued assets (0)')).toBeInTheDocument();
    expect(screen.getByText(/No issued assets observed/)).toBeInTheDocument();
    expect(
      screen.queryByText(/Issued-asset list unavailable/),
    ).not.toBeInTheDocument();
  });

  it('sums observations across the served assets', async () => {
    mockIssuer({
      g_strkey: G,
      home_domain: 'circle.com',
      assets: [
        {
          asset_id: `USDC-${G}`,
          code: 'USDC',
          slug: 'usdc',
          first_seen_ledger: 100,
          last_seen_ledger: 200,
          observation_count: 40,
        },
        {
          asset_id: `EURC-${G}`,
          code: 'EURC',
          slug: 'eurc',
          first_seen_ledger: 100,
          last_seen_ledger: 200,
          observation_count: 2,
        },
      ],
    });
    await renderPage();

    expect(screen.getByText('Assets').nextElementSibling?.textContent).toBe(
      '2',
    );
    expect(
      screen.getByText('Total observations').nextElementSibling?.textContent,
    ).toBe('42');
    expect(screen.getByText('Issued assets (2)')).toBeInTheDocument();
  });
});

// F086: functions/issuers/[[path]].js serves THIS route's baked static
// HTML verbatim for every issuer beyond the pre-rendered top-100 (a 404
// sub-fetches /issuers/shell/ and returns its body at 200). Whatever
// generateMetadata bakes for g_strkey === 'shell' is therefore what every
// long-tail issuer page declares to a crawler — it must be a generic
// noindex shell, never the real per-issuer metadata built from the
// literal string 'shell'.
describe('IssuerDetailPage generateMetadata', () => {
  it('builds real canonical/og:url metadata for a real issuer, indexable', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ g_strkey: G }),
    });
    expect(meta.alternates?.canonical).toBe(
      `${CURRENT_NETWORK.explorerUrl}/issuers/${G}`,
    );
    expect(meta.openGraph?.url).toBe(
      `${CURRENT_NETWORK.explorerUrl}/issuers/${G}`,
    );
    expect(meta.robots).toBeUndefined();
  });

  it('serves generic noindex metadata for the runtime-fallback shell, never a shell-URL canonical', async () => {
    const meta = await generateMetadata({
      params: Promise.resolve({ g_strkey: 'shell' }),
    });
    expect(meta.robots).toEqual({ index: false, follow: true });
    expect(meta.alternates?.canonical).toBeUndefined();
    expect(meta.openGraph?.url).toBeUndefined();
    expect(meta.title).not.toMatch(/shell/i);
  });
});
