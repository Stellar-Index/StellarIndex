import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { LendingProtocolStats } from './LendingProtocolStats';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

function renderBoth() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <section data-testid="blend">
        <LendingProtocolStats protocol="blend" />
      </section>
      <section data-testid="blnt">
        <LendingProtocolStats protocol="blnt" />
      </section>
    </QueryClientProvider>,
  );
}

const ROWS = [
  {
    protocol: 'blend',
    pool_count: 3,
    reserve_count: 9,
    total_supplied_usd: '12500000.00',
    total_borrowed_usd: '4000000.00',
    available_usd: '8500000.00',
    lower_bound: true,
    excluded: [
      {
        kind: 'reserve',
        pool: 'CPOOL',
        asset: 'EURC-GISSUER',
        reason: 'unpriced',
      },
    ],
    token: {
      asset_id: 'BLND-GISSUER',
      code: 'BLND',
      issuer: 'GISSUER',
      decimals: 7,
      price_usd: '0.05',
      circulating_supply: '1000000000000000',
      total_supply: '2000000000000000',
      market_cap_usd: '5000000.00',
    },
  },
  {
    protocol: 'blnt',
    pool_count: 1,
    reserve_count: 2,
    total_supplied_usd: '750000.00',
    total_borrowed_usd: '250000.00',
    available_usd: '500000.00',
    lower_bound: false,
    excluded: [],
    token: {
      asset_id: 'BLNT-GISSUER',
      code: 'BLNT',
      issuer: 'GISSUER',
      decimals: 7,
      price_usd: null,
      circulating_supply: null,
      total_supply: null,
      market_cap_usd: null,
    },
  },
];

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('LendingProtocolStats', () => {
  it('renders Blend and BLNT side by side from one protocols read', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: ROWS });
    renderBoth();
    const blnt = within(await screen.findByTestId('blnt'));
    await waitFor(() => expect(blnt.getByText('$750,000')).toBeInTheDocument());
    expect(
      within(screen.getByTestId('blend')).getByText('BLND price'),
    ).toBeInTheDocument();
    expect(blnt.getByText('BLNT price')).toBeInTheDocument();
    const protocolReads = vi
      .mocked(apiGet)
      .mock.calls.filter(([path]) =>
        String(path).includes('/lending/protocols'),
      );
    expect(protocolReads).toHaveLength(1);
  });

  it('renders a lower-bound total with ≥ and names what it excluded', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: ROWS });
    renderBoth();
    const blend = within(await screen.findByTestId('blend'));
    await waitFor(() =>
      expect(blend.getByTitle(/At least \$12,500,000\.00/)).toHaveTextContent(
        '≥',
      ),
    );
    expect(
      blend.getByText(/≥ lower bound: 1 reserve has no reliable price/),
    ).toBeInTheDocument();
    expect(blend.getByText(/no reliable price:/).nextSibling).toHaveTextContent(
      'EURC',
    );
  });

  it('says why the token price is missing instead of a bare dash', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: ROWS });
    renderBoth();
    const blnt = within(await screen.findByTestId('blnt'));
    await waitFor(() =>
      expect(
        blnt.getAllByText('no reliable market price yet').length,
      ).toBeGreaterThanOrEqual(2),
    );
  });

  it('renders a null USD as "—", never $0', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: ROWS });
    renderBoth();
    const blnt = within(await screen.findByTestId('blnt'));
    await waitFor(() =>
      expect(blnt.getByText('BLNT price')).toBeInTheDocument(),
    );
    expect(blnt.queryByText('$0')).toBeNull();
    expect(blnt.getAllByText('—').length).toBeGreaterThanOrEqual(3);
  });

  it('shows an error state when the read fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderBoth();
    await waitFor(() =>
      expect(
        screen.getAllByText(/Protocol totals are unavailable/).length,
      ).toBe(2),
    );
  });
});
