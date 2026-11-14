import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { IssuerPanel, ledgerAxis } from './IssuerPanel';

// The issuer writes its SEP-1 org_name and controls the toml that links it, so
// the panel never calls it the "Organisation": a linked domain shows it as a
// self-declared name, an unlinked one not at all. The home_domain always shows.
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function mockIssuer(org_verified: boolean) {
  vi.mocked(apiGet).mockResolvedValue({
    data: {
      g_strkey: G,
      org_name: 'Circle',
      home_domain: 'circle.com',
      org_verified,
      assets: [],
    },
  } as unknown as never);
}

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <IssuerPanel gStrkey={G} />
    </QueryClientProvider>,
  );
}

describe('IssuerPanel org attribution gating (CS-100)', () => {
  it('labels a domain-linked issuer’s org name as self-declared', async () => {
    mockIssuer(true);
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText('Self-declared name')).toBeInTheDocument(),
    );
    expect(screen.queryByText('Organisation')).not.toBeInTheDocument();
  });

  it('withholds the org name for an unverified issuer (shows home_domain instead)', async () => {
    mockIssuer(false);
    renderPanel();

    // Wait for the panel to load — the always-present "Home domain" field is
    // the resolved-state signal, so the negative assertions below run against
    // a fully-loaded panel rather than the "Loading…" placeholder.
    await waitFor(() =>
      expect(screen.getByText('Home domain')).toBeInTheDocument(),
    );

    // The borrowed org name must not appear anywhere in the panel.
    expect(screen.queryByText('Self-declared name')).not.toBeInTheDocument();
    expect(screen.queryByText('Circle')).not.toBeInTheDocument();
    // The real on-chain home_domain is still surfaced.
    expect(screen.getAllByText('circle.com').length).toBeGreaterThan(0);
  });
});

// Frontend-honesty sweep: /v1/issuers/{g} SOFT-FAILS its per-asset
// fan-out ("Soft-fail on the asset list … Includes deadline exceeded",
// internal/api/v1/issuers.go) and `assets` is omitempty — so an errored
// or timed-out read arrives as an absent field. `?? []` turned that into
// "No issued assets recorded." — a claim about the issuer we cannot make
// from a read that never answered.
describe('IssuerPanel issued-asset list', () => {
  it('says the list is unavailable when `assets` is absent', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { g_strkey: G, home_domain: 'circle.com', org_verified: true },
    } as unknown as never);
    renderPanel();
    await waitFor(() =>
      expect(
        screen.getByText(/Issued-asset list unavailable/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No issued assets recorded/),
    ).not.toBeInTheDocument();
  });

  it('keeps the genuine empty claim when `assets` is present and empty', async () => {
    mockIssuer(true);
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText(/No issued assets recorded/)).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/Issued-asset list unavailable/),
    ).not.toBeInTheDocument();
  });

  it('renders — not #0 — for an unknown first/last seen ledger', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        g_strkey: G,
        home_domain: 'circle.com',
        org_verified: true,
        assets: [
          {
            asset_id: `USDC-${G}`,
            code: 'USDC',
            slug: 'usdc',
            observation_count: 7,
          },
        ],
      },
    } as unknown as never);
    renderPanel();
    await waitFor(() => expect(screen.getByText('USDC')).toBeInTheDocument());
    // Ledger 0 cannot exist (genesis is 1).
    expect(screen.queryByText('#0')).not.toBeInTheDocument();
    expect(screen.getAllByText('—').length).toBeGreaterThanOrEqual(2);
  });
});

describe('issued-asset active span', () => {
  it('spans the earliest first-seen to the latest last-seen ledger', () => {
    expect(
      ledgerAxis([
        { first_seen_ledger: 200, last_seen_ledger: 300 },
        { first_seen_ledger: 100, last_seen_ledger: 250 },
        { first_seen_ledger: null, last_seen_ledger: 999 },
      ]),
    ).toEqual({ lo: 100, hi: 300 });
    expect(
      ledgerAxis([{ first_seen_ledger: 5, last_seen_ledger: 5 }]),
    ).toBeNull();
  });

  it('draws a span bar per asset with both ledgers known', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        g_strkey: G,
        org_verified: false,
        assets: [
          {
            asset_id: `USDC-${G}`,
            code: 'USDC',
            slug: 'usdc',
            observation_count: 7,
            first_seen_ledger: 100,
            last_seen_ledger: 300,
          },
          {
            asset_id: `EURC-${G}`,
            code: 'EURC',
            slug: 'eurc',
            observation_count: 2,
            first_seen_ledger: 250,
            last_seen_ledger: 300,
          },
        ],
      },
    } as unknown as never);
    renderPanel();
    expect(
      await screen.findByLabelText('Seen from ledger 250 to 300'),
    ).toBeInTheDocument();
    expect(screen.getByText('Active span')).toBeInTheDocument();
  });
});
