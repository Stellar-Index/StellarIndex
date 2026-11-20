import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { LendingRatesTable } from './LendingRatesTable';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const BLEND = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const BLNT = 'CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7';

const rate = (over: Record<string, unknown>) => ({
  protocol: 'blend',
  pool: BLEND,
  asset: 'CUSDC',
  asset_id: 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
  supply_apr: 0.05,
  borrow_apr: 0.08,
  utilization_pct: 50,
  supplied: '1',
  borrowed: '1',
  supplied_usd: '1000.00',
  borrowed_usd: '500.00',
  obtainable: true,
  exclusions: [],
  ...over,
});

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingRatesTable />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('LendingRatesTable', () => {
  it('lists Blend and BLNT reserves, filters by protocol, and renders a null USD as —', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        rates: [
          rate({}),
          rate({
            protocol: 'blnt',
            pool: BLNT,
            supply_apr: 0.07,
            supplied_usd: null,
          }),
        ],
        omitted_pools: [],
        partial: false,
      },
    });
    renderIt();
    await waitFor(() => expect(screen.getByText('7.00%')).toBeInTheDocument());
    expect(screen.getByText('5.00%')).toBeInTheDocument();
    expect(screen.queryByText('$0')).toBeNull();
    expect(screen.getByTitle('no reliable price')).toHaveTextContent('—');
    fireEvent.click(screen.getByRole('button', { name: /^BLNT/ }));
    expect(screen.queryByText('5.00%')).toBeNull();
    expect(screen.getByText('7.00%')).toBeInTheDocument();
  });

  it('drives the scatter from the protocol filter and notes unpriced reserves', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        rates: [
          rate({}),
          rate({
            protocol: 'blnt',
            pool: BLNT,
            asset: 'CX',
            asset_id: 'native',
          }),
          rate({
            protocol: 'blnt',
            pool: BLNT,
            asset: 'CY',
            supplied_usd: null,
          }),
        ],
        omitted_pools: [],
        partial: false,
      },
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getAllByTestId('scatter-dot')).toHaveLength(2),
    );
    expect(screen.getByText(/1 not priced, not shown/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^Blend/ }));
    expect(screen.getAllByTestId('scatter-dot')).toHaveLength(1);
    expect(screen.queryByText(/not priced/)).toBeNull();
  });

  it('hides non-obtainable rates by default, counts them, and marks them under All', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        rates: [
          rate({}),
          rate({
            asset: 'CFROZEN',
            asset_id: null,
            obtainable: false,
            exclusions: ['pool_not_active'],
          }),
        ],
      },
    });
    renderIt();
    expect(await screen.findByText('1 not obtainable hidden')).toBeVisible();
    expect(screen.getByText('Reserves (1)')).toBeInTheDocument();
    expect(screen.queryByLabelText(/not obtainable:/)).toBeNull();
    fireEvent.click(
      within(screen.getByRole('group', { name: 'Rates shown' })).getByRole(
        'button',
        { name: 'All' },
      ),
    );
    expect(screen.getByText('Reserves (2)')).toBeInTheDocument();
    expect(
      screen.getByLabelText('not obtainable: pool not active'),
    ).toBeInTheDocument();
    expect(screen.getByText(/marks a rate a new position/)).toBeVisible();
  });

  it('says a partial list is partial', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        rates: [rate({})],
        omitted_pools: [{ protocol: 'blnt', pool: BLNT, reason: 'x' }],
        partial: true,
      },
    });
    renderIt();
    await waitFor(() =>
      expect(screen.getByText(/Partial list/)).toBeInTheDocument(),
    );
  });

  it('shows an error state when the read fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderIt();
    await waitFor(() =>
      expect(
        screen.getByText(/Lending rates are unavailable/),
      ).toBeInTheDocument(),
    );
  });
});
