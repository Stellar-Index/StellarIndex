import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import type { components } from '@/api/types';
import {
  LendingProtocolEmissions,
  emissionsReasonText,
} from './LendingProtocolEmissions';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

type S = components['schemas'];
type Amounts = S['LendingProtocolEmissionAmounts'];
type Win = S['LendingProtocolEmissionsWindow'];
type Emissions = S['LendingProtocolEmissions'];

const NONE: Amounts = {
  amount_usd: null,
  lower_bound: false,
  usd_reason: 'not_valued',
  amount: null,
  amount_lower_bound: false,
  amount_reason: 'not_valued',
  approximate: false,
};

const exact = (
  usd: string,
  amount: string,
  o: Partial<Amounts> = {},
): Amounts => ({
  ...NONE,
  amount_usd: usd,
  usd_reason: null,
  amount,
  amount_reason: null,
  ...o,
});

const win = (s: Partial<Win['streams']> = {}, o: Partial<Win> = {}): Win => ({
  from_day: '2026-10-28',
  through_day: '2026-11-26',
  streams: {
    reserve_schedule: NONE,
    pool_claims: NONE,
    backstop_alloc_backstop: NONE,
    backstop_alloc_pool: NONE,
    backstop_claims: NONE,
    ...s,
  },
  excluded: [],
  ...o,
});

const block = (w: Win, o: Partial<Emissions> = {}): Emissions => ({
  degraded: false,
  emission_token: 'CTOKEN',
  emission_token_decimals: 7,
  '1d': w,
  '7d': w,
  '30d': w,
  all_time: w,
  ...o,
});

function serve(emissions: Emissions | undefined, protocol = 'blend') {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path === '/v1/sac-wrappers')
      return { data: { CTOKEN: 'BLND-GISSUER' } };
    return { data: [{ protocol, emissions }] };
  });
}

function renderPanel(protocol: 'blend' | 'blnt' = 'blend') {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingProtocolEmissions protocol={protocol} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(apiGet).mockReset();
});

describe('LendingProtocolEmissions', () => {
  it('renders exact figures one row per stream, never a total', async () => {
    serve(
      block(
        win({
          reserve_schedule: exact('1000.50', '12345678900000'),
          pool_claims: exact('300.00', '3000000000'),
        }),
      ),
    );
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(within(s).getAllByRole('term')).toHaveLength(5);
    expect(within(s).getByTitle('$1,000.50')).toBeInTheDocument();
    expect(within(s).getByTitle('$300.00')).toBeInTheDocument();
    expect(s).toHaveTextContent('not added together');
    expect(s).not.toHaveTextContent('$1,300.50');
    expect(screen.queryByTestId('emissions-excluded')).toBeNull();
    // Unscheduled streams are null with a reason, never $0.
    expect(s.textContent).not.toContain('$0');
  });

  it('switches window and unit, scaling tokens by decimals with the symbol', async () => {
    const w1 = win({ reserve_schedule: exact('10.00', '10000000') });
    const w30 = win({ reserve_schedule: exact('77.00', '770000000') });
    serve(block(w30, { '1d': w1 }));
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    fireEvent.click(await screen.findByRole('button', { name: 'BLND' }));
    expect(within(s).getByText('77 BLND')).toBeInTheDocument();
    expect(s.textContent).not.toContain('770000000');
    fireEvent.click(screen.getByRole('button', { name: '1d' }));
    expect(within(s).getByText('1 BLND')).toBeInTheDocument();
  });

  it('renders a lower bound as "at least"', async () => {
    serve(
      block(
        win(
          {
            reserve_schedule: exact('500.00', '5000000000', {
              lower_bound: true,
              amount_lower_bound: true,
            }),
          },
          {
            excluded: [
              {
                pool: 'CPOOL1',
                stream: null,
                reason: 'days_not_valued',
                count: 3,
              },
              {
                pool: 'CPOOL2',
                stream: null,
                reason: 'days_not_valued',
                count: 1,
              },
            ],
          },
        ),
      ),
    );
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(within(s).getByTitle(/^At least \$500\.00/)).toBeInTheDocument();
    expect(within(s).getAllByText('at least').length).toBeGreaterThan(0);
    expect(within(s).getByTestId('emissions-excluded')).toHaveTextContent(
      'some days are not valued yet: 2 pools',
    );
    fireEvent.click(await screen.findByRole('button', { name: 'BLND' }));
    expect(within(s).getByTitle(/^At least 500 BLND/)).toBeInTheDocument();
  });

  it.each([
    ['not_valued', 'not valued yet'],
    ['stream_unavailable', 'no valued pool serves this stream'],
    ['all_excluded', 'every day with an amount was left out'],
    ['emission_token_mixed', 'more than one token'],
    ['emission_token_not_stored', 'not recorded on the stored rows'],
    ['emission_token_decimals_unknown', 'decimals are not recorded'],
    ['emissions_read_failed', 'could not be read'],
    ['emissions_timed_out', 'took too long'],
    ['history_unverified', 'not verified complete'],
    ['history_verdict_stale', 'out of date'],
    ['history_verdict_unread', 'check could not be read'],
    ['days_not_valued', 'not valued yet'],
    ['valuation_stale', 're-valued'],
    ['price_missing', 'no price'],
  ])('maps reason %s to plain words', (code, text) => {
    expect(emissionsReasonText(code)).toContain(text);
    expect(emissionsReasonText(code)).not.toBe(code);
  });

  it('falls back to the raw string for an unknown reason', () => {
    expect(emissionsReasonText('brand_new_reason')).toBe('brand_new_reason');
  });

  it('shows a null USD with its reason, not zero', async () => {
    serve(
      block(
        win({
          reserve_schedule: exact('10.00', '100000000'),
          pool_claims: {
            ...NONE,
            usd_reason: 'all_excluded',
            amount_reason: 'emissions_read_failed',
          },
        }),
      ),
    );
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(s).toHaveTextContent('every day with an amount was left out');
    expect(s.textContent).not.toContain('$0');
    fireEvent.click(await screen.findByRole('button', { name: 'BLND' }));
    expect(s).toHaveTextContent('stored emissions could not be read');
  });

  it('marks an approximate stream, focusable, with the reason visible', async () => {
    serve(
      block(
        win({
          reserve_schedule: exact('9.00', '90000000', { approximate: true }),
        }),
        {
          emission_token: null,
          emission_token_decimals: null,
        },
      ),
      'blnt',
    );
    renderPanel('blnt');
    const s = await screen.findByTestId('emissions-blnt');
    const mark = within(s).getByRole('note');
    expect(mark).toHaveAttribute('tabindex', '0');
    expect(mark).toHaveAttribute(
      'title',
      expect.stringContaining('accurate to dust'),
    );
    expect(within(s).getByTestId('emissions-approx')).toBeInTheDocument();
  });

  it('says figures are unavailable when degraded', async () => {
    serve(
      block(win({ reserve_schedule: exact('10.00', '1') }), { degraded: true }),
    );
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(within(s).getByRole('status')).toHaveTextContent(
      'temporarily unavailable',
    );
    expect(s.textContent).not.toContain('$10.00');
  });

  it('shows one quiet line while nothing is valued', async () => {
    serve(block(win()));
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(within(s).getByRole('status')).toHaveTextContent('not computed yet');
    expect(s.textContent).not.toContain('$0');
  });

  it('BLNT: token amounts with USD withheld default to the token view', async () => {
    const withheld = (amount: string): Amounts => ({
      ...NONE,
      usd_reason: 'all_excluded',
      amount,
      amount_reason: null,
    });
    serve(
      block(win({ reserve_schedule: withheld('1230000000') }), {
        emission_token: 'CTOKEN',
      }),
      'blnt',
    );
    renderPanel('blnt');
    const s = await screen.findByTestId('emissions-blnt');
    expect(await within(s).findByText('123 BLND')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'USD' }));
    expect(s).toHaveTextContent('every day with an amount was left out');
    expect(s.textContent).not.toContain('$0');
  });

  it('Blend: USD with token amounts not stored renders both honestly', async () => {
    serve(
      block(
        win({
          reserve_schedule: {
            ...NONE,
            amount_usd: '55.00',
            usd_reason: null,
            amount_reason: 'emission_token_not_stored',
          },
        }),
        { emission_token: null, emission_token_decimals: null },
      ),
    );
    renderPanel();
    const s = await screen.findByTestId('emissions-blend');
    expect(within(s).getByTitle('$55.00')).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'emission token' }));
    expect(s).toHaveTextContent('not recorded on the stored rows');
    expect(s.textContent).not.toMatch(/\b0\b/);
  });

  it('never labels the token with a bare address', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) =>
      path === '/v1/sac-wrappers'
        ? { data: {} }
        : {
            data: [
              {
                protocol: 'blend',
                emissions: block(
                  win({ reserve_schedule: exact('1.00', '10000000') }),
                  { emission_token: 'C' + 'A'.repeat(55) },
                ),
              },
            ],
          },
    );
    renderPanel();
    await screen.findByTestId('emissions-blend');
    expect(
      screen.getByRole('button', { name: 'emission token' }),
    ).toBeInTheDocument();
  });

  it('renders nothing when the server sends no emissions block', async () => {
    serve(undefined);
    const { container } = renderPanel();
    await vi.waitFor(() => expect(container.textContent).toBe(''));
  });

  it('shows an error with retry', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('500 boom'));
    renderPanel();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Emissions are unavailable',
    );
  });
});
