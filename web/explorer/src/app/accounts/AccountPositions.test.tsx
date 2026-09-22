import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountPositions } from './AccountPositions';

// RLT-384. The panel valued a portfolio off /v1/price/batch typed as
// `{data: BatchPrice[]}` — `{asset_id, price}` and nothing else — under
// a hint that said the holdings were "valued at the live VWAP". Two of
// those words could be false at once: a `peg` row is the operator's
// standing 1:1 declaration rather than any observed market (r1 declares
// exactly one, Circle USDC), and the rows carry an `observed_at` that
// the panel never showed, so an hours-old valuation read as current.
// The envelope now rides through to the table.

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const ACCOUNT = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';

/** Balances are stroop integers (7 decimals, ADR-0003). */
const XLM_BALANCE = '1000000000'; // 100.0 XLM
const USDC_BALANCE = '500000000'; // 50.0 USDC

function hoursAgo(h: number): string {
  return new Date(Date.now() - h * 3_600_000).toISOString();
}

function stubApi() {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/accounts/')) {
      return {
        data: {
          account_id: ACCOUNT,
          exists: true,
          balance: XLM_BALANCE,
          trustlines: [{ asset: USDC, balance: USDC_BALANCE }],
        },
      };
    }
    if (path === '/v1/price/batch') {
      return {
        data: [
          {
            // The batch echoes native XLM as `crypto:XLM`.
            asset_id: 'crypto:XLM',
            quote: 'fiat:USD',
            price: '0.19498671210062048170',
            price_type: 'vwap',
            observed_at: hoursAgo(1),
            window_seconds: 60,
          },
          {
            asset_id: USDC,
            quote: 'fiat:USD',
            price: '1.000000000000',
            // Not an observation: the operator's declaration.
            price_type: 'peg',
            observed_at: hoursAgo(36),
          },
        ],
        as_of: new Date().toISOString(),
        flags: { stale: true },
      };
    }
    throw new Error(`unexpected path ${path}`);
  });
}

function renderPanel() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AccountPositions id={ACCOUNT} />
    </QueryClientProvider>,
  );
}

describe('AccountPositions price envelope', () => {
  it('labels a peg-valued holding rather than presenting it as a market price', async () => {
    stubApi();
    renderPanel();

    expect(await screen.findByText('peg')).toBeInTheDocument();
    // Exactly the one holding the API declared as a peg — the XLM row is
    // an observed VWAP and must stay unlabelled.
    expect(screen.getAllByText('peg')).toHaveLength(1);
  });

  it('stamps the valuation with the oldest observed_at and the API stale flag', async () => {
    stubApi();
    renderPanel();

    expect(
      await screen.findByText(
        /Prices observed 2d ago · flagged stale by the pricing API/,
      ),
    ).toBeInTheDocument();
  });

  it('keeps the portfolio total exact while carrying the envelope', async () => {
    stubApi();
    renderPanel();

    // 100 XLM × 0.19498671210062048170 + 50 USDC × 1 = 69.498671…
    // (the stat tile and the donut centre both carry it).
    expect(await screen.findAllByText('$69.50')).toHaveLength(2);
    // …and the per-holding values the total is built from (the donut
    // legend repeats them, so count rather than assume a single node).
    expect(screen.getAllByText('$50.00').length).toBeGreaterThan(0);
    expect(screen.getAllByText('$19.50').length).toBeGreaterThan(0);
  });

  it('sums the portfolio total in exact cents, not a re-floated Number multiply (RLT-069)', async () => {
    // Crafted so `Number(amount) * Number(price)` summed as floats rounds
    // the total UP to the next cent: float total = $675,005.24, exact
    // BigInt total (Σ of the two holdings' correctly-rounded cent values)
    // = $675,005.23. Either holding computed independently and correctly
    // rounded still sums, in float, to the wrong total.
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/accounts/')) {
        return {
          data: {
            account_id: ACCOUNT,
            exists: true,
            balance: '132764525', // 13.2764525 XLM
            trustlines: [{ asset: USDC, balance: '6852799660' }], // 685.279966 USDC
          },
        };
      }
      if (path === '/v1/price/batch') {
        return {
          data: [
            {
              asset_id: 'crypto:XLM',
              quote: 'fiat:USD',
              price: '676.749654447081070430',
              price_type: 'vwap',
              observed_at: hoursAgo(1),
            },
            {
              asset_id: USDC,
              quote: 'fiat:USD',
              price: '971.895334601568151811',
              price_type: 'vwap',
              observed_at: hoursAgo(1),
            },
          ],
          as_of: new Date().toISOString(),
          flags: { stale: false },
        };
      }
      throw new Error(`unexpected path ${path}`);
    });
    renderPanel();

    expect(await screen.findAllByText('$675,005.23')).not.toHaveLength(0);
    expect(screen.queryByText('$675,005.24')).not.toBeInTheDocument();
  });
});
