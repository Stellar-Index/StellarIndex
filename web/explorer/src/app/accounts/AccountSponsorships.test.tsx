import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountSponsorshipsPanel } from './AccountSponsorships';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

const totals = (inForce: number, reserves: number, started: number) => ({
  in_force: inForce,
  in_force_reserve_count: reserves,
  started,
  revoked: 1,
  ended: started - inForce - 1,
});

function resp(match: boolean | null) {
  return {
    data: {
      account: G,
      direction: 'sponsoring',
      state: 'in_force',
      arrangements: [],
      totals: {
        sponsoring: totals(1200, 1300, 1500),
        sponsored: totals(0, 0, 1),
      },
      proof: {
        account_ledger: 64802900,
        num_sponsoring: 1300,
        num_sponsored: 0,
        derived_sponsoring: match === false ? 1299 : 1300,
        derived_sponsored: 0,
        match,
      },
      through_ledger: 64802921,
      coverage_note: '',
    },
  };
}

describe('AccountSponsorshipsPanel', () => {
  it('shows the in-force totals and a matching proof', async () => {
    vi.mocked(apiGet).mockResolvedValueOnce(resp(true));
    renderWithClient(<AccountSponsorshipsPanel id={G} />);
    expect(await screen.findByText('1,200')).toBeTruthy();
    expect(screen.getByText('1,300 reserves')).toBeTruthy();
    expect(screen.getByText(/counters agree with these totals/)).toBeTruthy();
    expect(screen.getByText(/ledger #64802921/)).toBeTruthy();
    expect(vi.mocked(apiGet).mock.calls[0]?.[1]).toEqual({ limit: 1 });
  });

  it('says so when the counters disagree, never hiding the totals', async () => {
    vi.mocked(apiGet).mockResolvedValueOnce(resp(false));
    renderWithClient(<AccountSponsorshipsPanel id={G} />);
    expect(await screen.findByText(/the derived totals disagree/)).toBeTruthy();
    expect(screen.getByText('1,200')).toBeTruthy();
  });

  it('skips the comparison when the entry changed after the derivation', async () => {
    vi.mocked(apiGet).mockResolvedValueOnce(resp(null));
    renderWithClient(<AccountSponsorshipsPanel id={G} />);
    expect(await screen.findByText(/not compared/)).toBeTruthy();
  });

  it('reads a not-built 503 as not derived yet, not a failure', async () => {
    vi.mocked(apiGet).mockRejectedValueOnce(
      new Error(
        `503 Service Unavailable on /v1/accounts/${G}/sponsorships [https://api.stellarindex.io/errors/sponsorships-not-built]`,
      ),
    );
    renderWithClient(<AccountSponsorshipsPanel id={G} />);
    expect(await screen.findByText(/has not been derived/)).toBeTruthy();
  });
});
