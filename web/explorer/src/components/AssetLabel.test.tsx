import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetLabel } from './AssetLabel';

// AssetLabel resolves an issuer's SEP-1 org_name for the asset subtitle
// ("by {org}") across the markets / dexes / exchanges / pools tables. That
// attribution is only trustworthy when the org is SEP-1 VERIFIED
// (bidirectional — the org's stellar.toml lists the issuer back). org_name
// alone is self-declared: a scam issuer can point its on-chain home_domain
// at a reputable org's domain to borrow that org's ORG_NAME. Rendering it
// unqualified would launder the spoof (CS-100 / trust-spoofing audit).
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const VERIFIED_ISSUER = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const SPOOF_ISSUER = 'GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7ZLNK4RYNNKG';

// Both issuers self-declare the SAME reputable ORG_NAME; only one is
// bidirectionally verified. Exercises the exact spoof the audit describes.
function mockDirectory() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/issuers') {
      return {
        data: [
          {
            g_strkey: VERIFIED_ISSUER,
            org_name: 'Circle',
            home_domain: 'circle.com',
            org_verified: true,
          },
          {
            g_strkey: SPOOF_ISSUER,
            org_name: 'Circle',
            home_domain: 'circle.com',
            org_verified: false,
          },
        ],
      } as unknown as never;
    }
    // useSACWrappers also fires; keep it empty for these classic assets.
    return { data: {} } as unknown as never;
  });
}

function renderTree() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <div data-testid="verified">
        <AssetLabel canonical={`USDC-${VERIFIED_ISSUER}`} />
      </div>
      <div data-testid="spoof">
        <AssetLabel canonical={`USDC-${SPOOF_ISSUER}`} />
      </div>
    </QueryClientProvider>,
  );
}

describe('AssetLabel org attribution gating (CS-100)', () => {
  it('shows "by {org}" for a verified issuer but never for an unverified spoof', async () => {
    mockDirectory();
    renderTree();

    // Wait for the issuer directory to resolve — the VERIFIED label gaining
    // its attribution is the signal that the map is loaded and re-rendered.
    // (This makes the negative assertion below non-vacuous: it runs against
    // a fully-resolved directory, not the empty loading state.)
    await waitFor(() =>
      expect(
        within(screen.getByTestId('verified')).getByText(/by Circle/),
      ).toBeInTheDocument(),
    );

    // In that SAME resolved state, the unverified spoof must NOT present the
    // borrowed org name as authoritative attribution.
    const spoof = within(screen.getByTestId('spoof'));
    expect(spoof.queryByText(/by Circle/)).not.toBeInTheDocument();
    expect(spoof.queryByText(/Circle/)).not.toBeInTheDocument();
    // The asset code still renders for the spoof (only the attribution drops).
    expect(spoof.getByText('USDC')).toBeInTheDocument();
  });
});
