import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { BadDebtPanel } from './BadDebtPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const POOL = 'CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD';
const USDC_SAC = 'CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75';

type O = Record<string, unknown>;

const part = (o: O = {}) => ({
  d_tokens: '0',
  d_tokens_reason: null,
  underlying: '0',
  underlying_reason: null,
  usd: '0.00',
  usd_reason: null,
  events: 0,
  ...o,
});

const RECONCILED = {
  asset: USDC_SAC,
  moved_to_backstop: part({
    d_tokens: '1000000000',
    underlying: '1100000000',
    usd: '33.00',
    events: 1,
  }),
  covered_by_backstop: part({
    d_tokens: '600000000',
    underlying: '660000000',
    usd: '19.80',
    events: 1,
  }),
  socialised_to_suppliers: part(),
  repaid_by_backstop: part({
    d_tokens: '100000000',
    underlying: '110000000',
    usd: '3.30',
    events: 1,
  }),
  outstanding: part({
    d_tokens: '300000000',
    underlying: '330000000',
    usd: '9.90',
  }),
  reconciled: true,
  reconcile_reason: null,
  residual_d_tokens: '0',
};

const total = (usd: string | null, o: O = {}) => ({
  usd,
  lower_bound: false,
  excluded: [],
  ...o,
});

function payload(reserves: O[], o: O = {}) {
  return {
    data: {
      pool: POOL,
      protocol: 'blend',
      valued_at: 'event_time',
      backstop: 'CBACKSTOP',
      backstop_reason: null,
      position_ledger: 61341315,
      events_after_position: 0,
      events_written_through: 61341400,
      zero_bid_fills: 0,
      unrecorded_bid_fills: 0,
      reserves,
      totals_usd: {
        moved_to_backstop: total('33.00'),
        covered_by_backstop: total('19.80'),
        socialised_to_suppliers: total('0.00'),
        repaid_by_backstop: total('3.30'),
        outstanding: total('9.90'),
      },
      ...o,
    },
  };
}

function mockApi(badDebt: () => unknown) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/sac-wrappers')
      return {
        data: {
          [USDC_SAC]:
            'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        },
      };
    if (path.endsWith('/reserves'))
      return { data: { reserves: [{ asset: USDC_SAC, decimals: 7 }] } };
    return badDebt();
  });
}

function renderIt() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <BadDebtPanel pool={POOL} />
    </QueryClientProvider>,
  );
}

// The stat grid comes first; the proportion bars reuse the part labels.
const stat = (label: string) =>
  screen.getAllByText(label, { selector: 'span.truncate' })[0].parentElement
    ?.nextElementSibling as HTMLElement;
const check = () =>
  screen.getByTestId(`bad-debt-check-${USDC_SAC}`).textContent;

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('BadDebtPanel', () => {
  it('a reconciled pool shows five parts in USD and where the debt went', async () => {
    mockApi(() => payload([RECONCILED]));
    renderIt();
    await screen.findByText('Bad debt');
    expect(stat('Moved to backstop').textContent).toBe('$33.00');
    expect(stat('Covered by backstop').textContent).toBe('$19.80');
    expect(stat('Outstanding').textContent).toBe('$9.90');
    expect(check()).toBe('yes');
    await screen.findByText('Where USDC bad debt went');
    expect(screen.getByText('60.0%')).toBeInTheDocument();
    expect(screen.getByText('30.0%')).toBeInTheDocument();
    // Token amounts are secondary, under the USD figure.
    expect(
      screen.getByTestId(`bad-debt-moved_to_backstop-${USDC_SAC}`).textContent,
    ).toMatch(/^\$33\.00110/);
  });

  it('a pool whose parts do not add up shows the residual plainly and no proportion bar', async () => {
    mockApi(() =>
      payload([
        { ...RECONCILED, reconciled: false, residual_d_tokens: '50000000' },
      ]),
    );
    renderIt();
    await screen.findByText('Bad debt');
    await waitFor(() => expect(check()).toBe('no: 5 dTokens unaccounted for'));
    expect(screen.queryByText(/bad debt went/)).toBeNull();
  });

  it('an uncheckable pool says why, and a null part is a dash with its reason, never $0', async () => {
    mockApi(() =>
      payload(
        [
          {
            ...RECONCILED,
            outstanding: part({
              d_tokens: null,
              d_tokens_reason: 'backstop_role_unverified',
              underlying: null,
              underlying_reason: 'backstop_role_unverified',
              usd: null,
              usd_reason: 'backstop_role_unverified',
            }),
            reconciled: null,
            reconcile_reason: 'part_not_derivable',
            residual_d_tokens: null,
          },
        ],
        { protocol: 'blnt', backstop_reason: 'backstop_role_unverified' },
      ),
    );
    renderIt();
    await screen.findByText('Bad debt');
    expect(check()).toBe("can't be checked: BLNT's backstop address is not verified");
    expect(
      screen.getByTestId(`bad-debt-outstanding-${USDC_SAC}`).textContent,
    ).toBe("—BLNT's backstop address is not verified");
  });

  it('a pool whose event tables lag the chain says it cannot be checked yet', async () => {
    mockApi(() =>
      payload([
        {
          ...RECONCILED,
          reconciled: null,
          reconcile_reason: 'as_of_mismatch',
          residual_d_tokens: null,
        },
      ]),
    );
    renderIt();
    await screen.findByText('Bad debt');
    expect(check()).toBe(
      "can't be checked yet: our event tables are behind the chain",
    );
  });

  it('a lower-bound total says "at least"', async () => {
    mockApi(() =>
      payload([RECONCILED], {
        totals_usd: {
          ...payload([]).data.totals_usd,
          moved_to_backstop: total('33.00', {
            lower_bound: true,
            excluded: [{ asset: USDC_SAC, reason: 'price_thin_market' }],
          }),
        },
      }),
    );
    renderIt();
    await screen.findByText('Bad debt');
    expect(stat('Moved to backstop').textContent).toBe('≥at least $33.00');
    expect(stat('Covered by backstop').textContent).toBe('$19.80');
  });

  it('a pool with true zeros renders nothing', async () => {
    mockApi(() =>
      payload([
        {
          asset: USDC_SAC,
          moved_to_backstop: part(),
          covered_by_backstop: part(),
          socialised_to_suppliers: part(),
          repaid_by_backstop: part(),
          outstanding: part(),
          reconciled: true,
          reconcile_reason: null,
          residual_d_tokens: '0',
        },
      ]),
    );
    const { container } = renderIt();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(container.textContent).toBe('');
  });

  it('no events but an unknown outstanding is one quiet line, not silence', async () => {
    mockApi(() =>
      payload([
        {
          asset: USDC_SAC,
          moved_to_backstop: part(),
          covered_by_backstop: part(),
          socialised_to_suppliers: part(),
          repaid_by_backstop: part(),
          outstanding: part({
            d_tokens: null,
            d_tokens_reason: 'backstop_role_unverified',
            usd: null,
            usd_reason: 'backstop_role_unverified',
          }),
          reconciled: null,
          reconcile_reason: 'part_not_derivable',
          residual_d_tokens: null,
        },
      ]),
    );
    renderIt();
    expect(
      await screen.findByText(/No bad-debt auction recorded for this pool/),
    ).toHaveTextContent("can't be checked: BLNT's backstop address is not verified.");
    expect(screen.queryByRole('table')).toBeNull();
  });

  it('a failed fetch says so; a 404 renders no panel', async () => {
    mockApi(() => Promise.reject(new Error('500 Internal Server Error')));
    const failed = renderIt();
    await screen.findByText(/Bad debt is unavailable right now/);
    failed.unmount();
    mockApi(() => Promise.reject(new Error('404 Not Found')));
    const { container } = renderIt();
    await waitFor(() => expect(apiGet).toHaveBeenCalled());
    await new Promise((r) => setTimeout(r, 20));
    expect(container.textContent).toBe('');
  });
});
