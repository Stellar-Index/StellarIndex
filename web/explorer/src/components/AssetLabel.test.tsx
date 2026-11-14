import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetLabel } from './AssetLabel';

// AssetLabel's subtitle shows the issuer's home domain when the toml there
// lists the issuer back, and never the SEP-1 org_name: the issuer writes that
// name and controls both ends of the toml link, so a scam issuer with its own
// domain can claim any organisation's name and still pass the check.
vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const VERIFIED_ISSUER =
  'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const SPOOF_ISSUER = 'GBUYUAI75XXWDZEKLY66CFYKQPET5JR4EENXZBUZ3YXZ7ZLNK4RYNNKG';

// Both issuers claim the same reputable ORG_NAME; only one home domain lists
// its issuer back.
function mockDirectory() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/issuers') {
      return {
        data: [
          {
            g_strkey: VERIFIED_ISSUER,
            org_name: 'Circle',
            home_domain: 'centre-usdc.example',
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

describe('AssetLabel issuer subtitle', () => {
  it('shows the linked home domain, and the claimed org name for no issuer', async () => {
    mockDirectory();
    renderTree();

    // The linked label gaining its domain is the signal that the directory
    // has loaded, so the negative assertions below are not vacuous.
    const linked = within(screen.getByTestId('verified'));
    await waitFor(() =>
      expect(linked.getByText('centre-usdc.example')).toBeInTheDocument(),
    );
    // A self-listing toml proves the domain, not the name it claims.
    expect(linked.queryByText(/Circle/)).not.toBeInTheDocument();

    const spoof = within(screen.getByTestId('spoof'));
    expect(spoof.queryByText(/Circle/)).not.toBeInTheDocument();
    expect(spoof.queryByText(/circle\.com/)).not.toBeInTheDocument();
    // The asset code still renders for the spoof (only the attribution drops).
    expect(spoof.getByText('USDC')).toBeInTheDocument();
  });
});

// A Soroban pool pairs against SAC contract addresses, not
// classic asset ids. Two DISTINCT issuers can share a code (a scam SAC
// wrapping "USDC-<attacker issuer>" alongside the real "USDC-<Circle
// issuer>"): the resolved-SAC branch must disambiguate on the issuer,
// exactly like the classic branch does, or both pool rows render the
// byte-identical "USDC / SAC" with no way to tell them apart.
const REAL_SAC_CONTRACT = 'CA' + 'A'.repeat(54); // 56-char C-strkey shape
const SCAM_SAC_CONTRACT = 'CB' + 'B'.repeat(54); // 56-char C-strkey shape

function mockSACCollision() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/issuers') {
      return { data: [] } as unknown as never;
    }
    if (path === '/v1/sac-wrappers') {
      return {
        data: {
          [REAL_SAC_CONTRACT]: `USDC-${VERIFIED_ISSUER}`,
          [SCAM_SAC_CONTRACT]: `USDC-${SPOOF_ISSUER}`,
        },
      } as unknown as never;
    }
    return { data: {} } as unknown as never;
  });
}

describe('AssetLabel SAC-resolved issuer disambiguation (CA2-A36)', () => {
  it('shows distinct issuer identity for two SAC contracts sharing a code', async () => {
    mockSACCollision();
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <div data-testid="real">
          <AssetLabel canonical={REAL_SAC_CONTRACT} />
        </div>
        <div data-testid="scam">
          <AssetLabel canonical={SCAM_SAC_CONTRACT} />
        </div>
      </QueryClientProvider>,
    );

    // Both resolve to the same code ("USDC / SAC"), so the only way to
    // tell them apart is the issuer. Wait for resolution, then assert
    // each label carries its OWN issuer identity, not the bare code.
    await waitFor(() =>
      expect(within(screen.getByTestId('real')).getByTitle(VERIFIED_ISSUER)),
    );
    expect(
      within(screen.getByTestId('scam')).getByTitle(SPOOF_ISSUER),
    ).toBeInTheDocument();
    // The real and scam issuer must NOT be interchangeable in either cell.
    expect(
      within(screen.getByTestId('real')).queryByTitle(SPOOF_ISSUER),
    ).not.toBeInTheDocument();
    expect(
      within(screen.getByTestId('scam')).queryByTitle(VERIFIED_ISSUER),
    ).not.toBeInTheDocument();
  });
});
