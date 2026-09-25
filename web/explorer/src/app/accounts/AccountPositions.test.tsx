import { describe, it, expect, vi, afterEach } from 'vitest';
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
const UNPRICED_ASSET =
  'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';
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
  afterEach(() => {
    vi.useRealTimers();
  });

  // RLT-387: the /v1/price/batch query had staleTime only, no
  // refetchInterval — an open tab's valuation never updated again
  // without the visitor navigating away and back. It must poll live,
  // the same as the converter's identical batch read.
  it('re-fetches the price batch on a live interval, not staleTime alone', async () => {
    stubApi();
    vi.useFakeTimers();
    renderPanel();

    const callCount = () =>
      vi.mocked(apiGet).mock.calls.filter((c) => c[0] === '/v1/price/batch')
        .length;

    await vi.waitFor(() => expect(callCount()).toBe(1));

    await vi.advanceTimersByTimeAsync(60_000);

    await vi.waitFor(() => expect(callCount()).toBeGreaterThan(1));
  });

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

  it('renders a total with an unpriced holding as a lower bound naming the exclusion', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/accounts/')) {
        return {
          data: {
            account_id: ACCOUNT,
            exists: true,
            balance: XLM_BALANCE,
            trustlines: [
              { asset: USDC, balance: USDC_BALANCE },
              // A real positive balance the pricing API won't price.
              { asset: UNPRICED_ASSET, balance: '70000000000' },
            ],
          },
        };
      }
      if (path === '/v1/price/batch') {
        return {
          data: [
            {
              asset_id: 'crypto:XLM',
              quote: 'fiat:USD',
              price: '0.19498671210062048170',
              price_type: 'vwap',
              observed_at: hoursAgo(1),
            },
            {
              asset_id: USDC,
              quote: 'fiat:USD',
              price: '1.000000000000',
              price_type: 'vwap',
              observed_at: hoursAgo(1),
            },
            { asset_id: UNPRICED_ASSET, quote: 'fiat:USD', price: null },
          ],
          as_of: new Date().toISOString(),
          flags: { stale: false },
        };
      }
      throw new Error(`unexpected path ${path}`);
    });
    renderPanel();

    // Stat tile and donut centre both carry the floor, never a bare total.
    expect(await screen.findAllByText('≥ $69.50')).toHaveLength(2);
    expect(screen.queryByText('$69.50')).not.toBeInTheDocument();
    expect(screen.getByText('excludes 1 unpriced')).toBeInTheDocument();
    expect(screen.getByText('value · excludes 1 unpriced')).toBeInTheDocument();
    // Shares are of the priced subtotal, and say so.
    expect(screen.getByText('71.9% of priced value')).toBeInTheDocument();
    expect(screen.getByText('Allocation (priced)')).toBeInTheDocument();
  });

  it('keeps a fully priced total unqualified', async () => {
    stubApi();
    renderPanel();

    expect(await screen.findAllByText('$69.50')).toHaveLength(2);
    expect(screen.queryByText(/≥/)).not.toBeInTheDocument();
    expect(screen.queryByText(/unpriced/)).not.toBeInTheDocument();
    expect(screen.getByText('71.9% of value')).toBeInTheDocument();
  });
});

// /v1/price/batch rejects the WHOLE request on one id it cannot parse and
// on more than 100 ids (internal/api/v1/price.go). The lake serves a
// classic AMM pool share as a `pool:<hex>` trustline, so one LP position
// used to erase the valuation of every other holding, silently.
const POOL_SHARE = `pool:${'ab'.repeat(32)}`;
const FILLER_ISSUER =
  'GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';

type Params = Record<string, string | number | undefined> | undefined;

function batchIdsOf(params: Params): string[] {
  return String(params?.asset_ids ?? '').split(',');
}

function stubStrictBatch(
  trustlines: { asset: string; balance: string }[],
  opts: { rejectAll?: boolean } = {},
) {
  vi.mocked(apiGet).mockImplementation(
    async (path: string, params?: Params) => {
      if (path.startsWith('/v1/accounts/')) {
        return {
          data: {
            account_id: ACCOUNT,
            exists: true,
            balance: XLM_BALANCE,
            trustlines,
          },
        };
      }
      if (path === '/v1/price/batch') {
        const ids = batchIdsOf(params);
        if (
          opts.rejectAll ||
          ids.length > 100 ||
          ids.some((id) => id.startsWith('pool:') || id === 'unknown_asset')
        ) {
          throw new Error(
            '400 Bad Request on /v1/price/batch — invalid-asset-id',
          );
        }
        const rows = [];
        if (ids.includes('native')) {
          rows.push({
            asset_id: 'crypto:XLM',
            quote: 'fiat:USD',
            price: '0.19498671210062048170',
            price_type: 'vwap',
            observed_at: hoursAgo(1),
          });
        }
        if (ids.includes(USDC)) {
          rows.push({
            asset_id: USDC,
            quote: 'fiat:USD',
            price: '1.000000000000',
            price_type: 'vwap',
            observed_at: hoursAgo(1),
          });
        }
        return {
          data: rows,
          as_of: new Date().toISOString(),
          flags: { stale: false },
        };
      }
      throw new Error(`unexpected path ${path}`);
    },
  );
}

function batchCalls(): string[][] {
  return vi
    .mocked(apiGet)
    .mock.calls.filter((c) => c[0] === '/v1/price/batch')
    .map((c) => batchIdsOf(c[1] as Params));
}

describe('AccountPositions batch robustness', () => {
  afterEach(() => {
    vi.mocked(apiGet).mockReset();
  });

  it('values the priced holdings when the account holds a pool share', async () => {
    stubStrictBatch([
      { asset: USDC, balance: USDC_BALANCE },
      { asset: POOL_SHARE, balance: '1230000000' },
      { asset: 'unknown_asset', balance: '10' },
    ]);
    renderPanel();

    expect(await screen.findAllByText('≥ $69.50')).toHaveLength(2);
    expect(screen.getByText('2 priced')).toBeInTheDocument();
    // The pool share stays listed, counted as unpriced rather than dropped.
    expect(screen.getByText('excludes 2 unpriced')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(batchCalls().flat()).not.toContain(POOL_SHARE);
  });

  it('chunks more than 100 holdings instead of losing the whole valuation', async () => {
    const filler = Array.from({ length: 119 }, (_, i) => ({
      asset: `T${i}-${FILLER_ISSUER}`,
      balance: '10000000',
    }));
    stubStrictBatch([{ asset: USDC, balance: USDC_BALANCE }, ...filler]);
    renderPanel();

    expect(await screen.findAllByText('≥ $69.50')).toHaveLength(2);
    expect(screen.getByText('2 priced')).toBeInTheDocument();
    const calls = batchCalls();
    expect(calls.length).toBe(2);
    for (const ids of calls) expect(ids.length).toBeLessThanOrEqual(100);
    expect(calls.flat()).toHaveLength(121);
  });

  it('says so when the price lookup itself failed', async () => {
    stubStrictBatch([{ asset: USDC, balance: USDC_BALANCE }], {
      rejectAll: true,
    });
    renderPanel();

    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Price lookup failed for 2 of 2 holdings',
    );
  });
});
