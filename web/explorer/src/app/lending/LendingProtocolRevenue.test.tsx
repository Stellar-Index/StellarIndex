import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  fireEvent,
  render,
  screen,
  waitFor,
  within,
} from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { components } from '@/api/types';
import { LendingProtocolRevenue } from './LendingProtocolRevenue';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

type S = components['schemas'];
type Amounts = S['LendingProtocolRevenueAmounts'];
type Win = S['LendingProtocolRevenueWindow'];
type Revenue = S['LendingProtocolRevenue'];

const empty: Amounts = {
  interest_accrued_usd: null,
  supplier_share_usd: null,
  backstop_take_usd: null,
  protocol_fee_usd: null,
  backstop_drawn_usd: null,
  backstop_drawn_lower_bound: true,
  default_loss_usd: null,
  lower_bound: false,
  usd_reason: 'not_valued',
};

const valued = (interest: string, o: Partial<Amounts> = {}): Amounts => ({
  ...empty,
  interest_accrued_usd: interest,
  supplier_share_usd: '800.00',
  backstop_take_usd: '150.00',
  protocol_fee_usd: '0.00',
  backstop_drawn_usd: '20.00',
  default_loss_usd: '0.00',
  usd_reason: null,
  ...o,
});

const win = (a: Amounts, o: Partial<Win> = {}): Win => ({
  ...a,
  from_day: a.usd_reason === 'not_valued' ? null : '2026-10-28',
  through_day: a.usd_reason === 'not_valued' ? null : '2026-11-26',
  excluded: [],
  versions: null,
  ...o,
});

const notValued: Revenue = {
  fee_rate_inferred: false,
  degraded: false,
  '1d': win(empty),
  '7d': win(empty),
  '30d': win(empty),
  all_time: win(empty),
};

function serve(rows: { protocol: 'blend' | 'blnt'; revenue?: Revenue }[]) {
  vi.mocked(apiGet).mockResolvedValue({ data: rows });
}

function renderPanel(protocol: 'blend' | 'blnt' = 'blend') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingProtocolRevenue protocol={protocol} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('LendingProtocolRevenue', () => {
  it.each(['blend', 'blnt'] as const)(
    'shows one quiet line for %s while nothing is valued, with no switcher or $0',
    async (p) => {
      serve([
        { protocol: 'blend', revenue: notValued },
        {
          protocol: 'blnt',
          revenue: { ...notValued, fee_rate_inferred: true },
        },
      ]);
      const { unmount } = renderPanel(p);
      const s = await screen.findByTestId(`revenue-${p}`);
      expect(within(s).getByRole('status')).toHaveTextContent(
        'Revenue totals are not computed yet.',
      );
      expect(s.textContent).not.toContain('—');
      expect(s.textContent).not.toContain('$0');
      expect(screen.queryByRole('group', { name: 'Window' })).toBeNull();
      unmount();
    },
  );

  it('shows only the protocol it is given', async () => {
    serve([
      { protocol: 'blend', revenue: notValued },
      { protocol: 'blnt', revenue: notValued },
    ]);
    renderPanel('blnt');
    expect(await screen.findByTestId('revenue-blnt')).toBeInTheDocument();
    expect(screen.queryByTestId('revenue-blend')).toBeNull();
  });

  it('renders each window from the API and switches between them', async () => {
    serve([
      {
        protocol: 'blend',
        revenue: {
          ...notValued,
          '1d': win(valued('11.11')),
          '7d': win(valued('77.77')),
          '30d': win(valued('3030.30')),
          all_time: win(valued('99999.99')),
        },
      },
    ]);
    renderPanel();
    const s = await screen.findByTestId('revenue-blend');
    expect(within(s).getByTitle('$3,030.30')).toBeInTheDocument();
    expect(s).toHaveTextContent('through Nov 26, 2026');
    for (const [label, exact] of [
      ['1d', '$11.11'],
      ['7d', '$77.77'],
      ['All time', '$99,999.99'],
      ['30d', '$3,030.30'],
    ]) {
      fireEvent.click(screen.getByRole('button', { name: label }));
      expect(within(s).getByTitle(exact)).toBeInTheDocument();
    }
    // Interest auction draws are always a floor.
    expect(within(s).getByTitle(/^At least \$20\.00/)).toBeInTheDocument();
    // Blend has no protocol fee row.
    expect(s).not.toHaveTextContent('Protocol fee');
  });

  it('marks a lower bound "at least" and counts what was excluded by reason', async () => {
    serve([
      {
        protocol: 'blend',
        revenue: {
          ...notValued,
          '30d': win(valued('500.00', { lower_bound: true }), {
            excluded: [
              { pool: 'CPOOL1', asset: 'CA1', reason: 'withheld', count: 4 },
              { pool: 'CPOOL2', asset: 'CA1', reason: 'withheld', count: 2 },
              {
                pool: 'CPOOL3',
                asset: null,
                reason: 'not_valued',
                count: null,
              },
            ],
          }),
        },
      },
    ]);
    renderPanel();
    const s = await screen.findByTestId('revenue-blend');
    expect(within(s).getByTitle(/^At least \$500\.00/)).toBeInTheDocument();
    expect(within(s).getAllByText('at least').length).toBeGreaterThan(0);
    const list = within(s).getByTestId('revenue-excluded');
    expect(list).toHaveTextContent(
      'price withheld (flagged issuer or thin market): 2 reserves',
    );
    expect(list).toHaveTextContent('not valued yet: 1 pool');
    expect(list.textContent).not.toContain('CPOOL');
  });

  it('shows a null window as a dash-free reason line and a null version as a dash, never $0', async () => {
    serve([
      {
        protocol: 'blend',
        revenue: {
          ...notValued,
          '30d': win(valued('40.00'), {
            versions: { v1: { ...empty }, v2: valued('40.00') },
          }),
          '7d': win(
            { ...empty, usd_reason: 'all_excluded', lower_bound: true },
            {
              excluded: [
                {
                  pool: 'CPOOL1',
                  asset: 'CA1',
                  reason: 'valuation_stale',
                  count: 7,
                },
              ],
            },
          ),
        },
      },
    ]);
    renderPanel();
    const s = await screen.findByTestId('revenue-blend');
    const v = within(s).getByTestId('revenue-versions');
    expect(within(v).getByTitle('not valued yet')).toHaveTextContent('—');
    expect(v.textContent).not.toContain('$0');
    fireEvent.click(screen.getByRole('button', { name: '7d' }));
    expect(within(s).getByRole('status')).toHaveTextContent(
      'Revenue cannot be shown for this window: every day with an amount was left out.',
    );
    expect(within(s).getByTestId('revenue-excluded')).toHaveTextContent(
      'some days are waiting to be re-valued: 1 reserve',
    );
    expect(s.textContent).not.toContain('$0');
  });

  it('shows the API total beside the versions, never their sum', async () => {
    serve([
      {
        protocol: 'blend',
        revenue: {
          ...notValued,
          '30d': win(valued('100.01'), {
            versions: { v1: valued('50.00'), v2: valued('50.00') },
          }),
        },
      },
    ]);
    renderPanel();
    const s = await screen.findByTestId('revenue-blend');
    expect(within(s).getByTitle('$100.01')).toBeInTheDocument();
    expect(within(s).queryByTitle('$100.00')).toBeNull();
    const v = within(s).getByTestId('revenue-versions');
    expect(v).toHaveTextContent('V1');
    expect(v).toHaveTextContent('V2');
    expect(within(v).getAllByTitle('$50.00')).toHaveLength(2);
  });

  it('labels the BLNT protocol fee as an inferred rate and notes a degraded read', async () => {
    serve([
      {
        protocol: 'blnt',
        revenue: {
          ...notValued,
          fee_rate_inferred: true,
          degraded: true,
          '30d': win(valued('900.00', { protocol_fee_usd: '45.00' })),
        },
      },
    ]);
    renderPanel('blnt');
    const s = await screen.findByTestId('revenue-blnt');
    expect(s).toHaveTextContent('Protocol fee (inferred rate)');
    expect(within(s).getByTitle('$45.00')).toBeInTheDocument();
    expect(s).toHaveTextContent('Some values could not be read.');
  });

  it('offers a retry when the read fails', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('500 Internal Server Error'));
    renderPanel();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Revenue is unavailable right now.',
    );
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument();
  });

  it('renders nothing on a 404 or when no protocol carries a revenue block', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('404 Not Found'));
    const { container, unmount } = renderPanel();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    await waitFor(() => expect(container).toBeEmptyDOMElement());
    unmount();
    serve([{ protocol: 'blend' }]);
    const r = renderPanel();
    await waitFor(() => expect(r.container).toBeEmptyDOMElement());
  });
});
