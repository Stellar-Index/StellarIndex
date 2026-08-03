import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { IssuerPanel } from './IssuerPanel';

// The Issuer-identity panel surfaces the issuer's SEP-1 org_name. That name is
// only an authoritative organisation identity when SEP-1 VERIFIED
// (bidirectional). Unverified it is self-declared metadata a scam issuer can
// spoof (set home_domain to a reputable org's domain to borrow its ORG_NAME),
// so it must not headline the panel or appear as the "Organisation" field
// (CS-100 / trust-spoofing audit). The real on-chain home_domain still shows.
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
  it('shows the Organisation field for a verified issuer', async () => {
    mockIssuer(true);
    renderPanel();
    await waitFor(() =>
      expect(screen.getByText('Organisation')).toBeInTheDocument(),
    );
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

    // The unverified, borrowed org name must not appear anywhere — neither as
    // the "Organisation" field nor as the panel's identity hint.
    expect(screen.queryByText('Organisation')).not.toBeInTheDocument();
    expect(screen.queryByText('Circle')).not.toBeInTheDocument();
    // The real on-chain home_domain is still surfaced.
    expect(screen.getAllByText('circle.com').length).toBeGreaterThan(0);
  });
});
