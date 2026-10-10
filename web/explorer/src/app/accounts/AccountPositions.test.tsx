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

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const UNPRICED =
  'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA';
const ACCOUNT = 'GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC';
// /v1/price/batch rejects the whole request on an unparseable id.
const POOL_SHARE = `pool:${'ab'.repeat(32)}`;

/** Balances are stroop integers (7 decimals, ADR-0003). */
const XLM_BALANCE = '1000000000'; // 100 XLM
const USDC_BALANCE = '500000000'; // 50 USDC

type Trustline = { asset: string; balance: string };
type Batch = (ids: string[]) => Record<string, unknown>;
type Params = Record<string, string | number | undefined> | undefined;

const hoursAgo = (h: number) =>
  new Date(Date.now() - h * 3_600_000).toISOString();

const row = (asset_id: string, price: string | null, over = {}) => ({
  asset_id,
  quote: 'fiat:USD',
  price,
  price_type: 'vwap',
  observed_at: hoursAgo(1),
  ...over,
});
// The batch echoes native XLM as `crypto:XLM`.
const XLM_ROW = row('crypto:XLM', '0.19498671210062048170');
const USDC_ROW = row(USDC, '1.000000000000');

// USDC valued by the operator's 1:1 declaration, 36h old, flagged stale.
const pegBatch: Batch = () => ({
  data: [
    XLM_ROW,
    row(USDC, '1.000000000000', {
      price_type: 'peg',
      observed_at: hoursAgo(36),
    }),
  ],
  flags: { stale: true },
});

// Mirrors the server: 400 on a pool share, an unknown id, or >100 ids.
const strictBatch =
  (rejectAll = false): Batch =>
  (ids) => {
    if (
      rejectAll ||
      ids.length > 100 ||
      ids.some((id) => id.startsWith('pool:') || id === 'unknown_asset')
    ) {
      throw new Error('400 Bad Request on /v1/price/batch — invalid-asset-id');
    }
    return {
      data: [
        ...(ids.includes('native') ? [XLM_ROW] : []),
        ...(ids.includes(USDC) ? [USDC_ROW] : []),
      ],
    };
  };

const idsOf = (params: Params) => String(params?.asset_ids ?? '').split(',');

function stubApi({
  balance = XLM_BALANCE,
  trustlines = [{ asset: USDC, balance: USDC_BALANCE }],
  batch = pegBatch,
}: { balance?: string; trustlines?: Trustline[]; batch?: Batch } = {}) {
  vi.mocked(apiGet).mockImplementation(
    async (path: string, params?: Params) => {
      if (path.startsWith('/v1/accounts/')) {
        return {
          data: { account_id: ACCOUNT, exists: true, balance, trustlines },
        };
      }
      if (path === '/v1/price/batch') {
        return {
          as_of: new Date().toISOString(),
          flags: { stale: false },
          ...batch(idsOf(params)),
        };
      }
      throw new Error(`unexpected path ${path}`);
    },
  );
}

type Text = string | RegExp;
const shows = (...t: Text[]) =>
  t.forEach((x) => expect(screen.getByText(x)).toBeInTheDocument());
const hides = (...t: Text[]) =>
  t.forEach((x) => expect(screen.queryByText(x)).not.toBeInTheDocument());

const batchCalls = () =>
  vi
    .mocked(apiGet)
    .mock.calls.filter((c) => c[0] === '/v1/price/batch')
    .map((c) => idsOf(c[1] as Params));

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

afterEach(() => {
  vi.useRealTimers();
  vi.mocked(apiGet).mockReset();
});

describe('AccountPositions price envelope', () => {
  it('re-fetches the price batch on a live interval, not staleTime alone', async () => {
    stubApi();
    vi.useFakeTimers();
    renderPanel();
    await vi.waitFor(() => expect(batchCalls()).toHaveLength(1));
    await vi.advanceTimersByTimeAsync(60_000);
    await vi.waitFor(() => expect(batchCalls().length).toBeGreaterThan(1));
  });

  it('values a fully priced portfolio exactly, labels the peg and stamps the envelope', async () => {
    stubApi();
    renderPanel();
    // 100 × 0.19498671210062048170 + 50 × 1; stat tile and donut centre.
    expect(await screen.findAllByText('$69.50')).toHaveLength(2);
    for (const v of ['$50.00', '$19.50'])
      expect(screen.getAllByText(v)).not.toHaveLength(0);
    hides(/≥|unpriced/);
    // Only USDC is a peg; the XLM VWAP stays unlabelled.
    expect(screen.getAllByText('peg')).toHaveLength(1);
    // Oldest observed_at plus the API's stale flag.
    shows(
      '71.9% of value',
      /Prices observed 2d ago · flagged stale by the pricing API/,
    );
  });

  it('sums the total in exact cents, not a re-floated Number multiply', async () => {
    // Float sum of the two holdings rounds up to $675,005.24.
    stubApi({
      balance: '132764525',
      trustlines: [{ asset: USDC, balance: '6852799660' }],
      batch: () => ({
        data: [
          row('crypto:XLM', '676.749654447081070430'),
          row(USDC, '971.895334601568151811'),
        ],
      }),
    });
    renderPanel();
    expect(await screen.findAllByText('$675,005.23')).not.toHaveLength(0);
    hides('$675,005.24');
  });

  it('renders a total with an unpriced holding as a lower bound naming the exclusion', async () => {
    stubApi({
      trustlines: [
        { asset: USDC, balance: USDC_BALANCE },
        { asset: UNPRICED, balance: '70000000000' },
      ],
      batch: () => ({ data: [XLM_ROW, USDC_ROW, row(UNPRICED, null)] }),
    });
    renderPanel();
    expect(await screen.findAllByText('≥ $69.50')).toHaveLength(2);
    hides('$69.50');
    shows('excludes 1 unpriced', 'value · excludes 1 unpriced');
    shows('71.9% of priced value', 'Allocation (priced)');
  });

  // Seen-and-refused is a different fact from never observed.
  it('labels a withheld holding distinctly from a never-observed one', async () => {
    stubApi({ batch: () => ({ data: [XLM_ROW], withheld: [USDC] }) });
    renderPanel();
    expect(await screen.findByText('withheld')).toBeInTheDocument();
    shows('excludes 1 withheld');
    hides(/unpriced/);
  });
});

describe('AccountPositions batch robustness', () => {
  it('values the priced holdings when the account holds a pool share', async () => {
    stubApi({
      trustlines: [
        { asset: USDC, balance: USDC_BALANCE },
        { asset: POOL_SHARE, balance: '1230000000' },
        { asset: 'unknown_asset', balance: '10' },
      ],
      batch: strictBatch(),
    });
    renderPanel();
    expect(await screen.findAllByText('≥ $69.50')).toHaveLength(2);
    shows('2 priced', 'excludes 2 unpriced');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(batchCalls().flat()).not.toContain(POOL_SHARE);
  });

  it('chunks more than 100 holdings instead of losing the whole valuation', async () => {
    const filler = Array.from({ length: 119 }, (_, i) => ({
      asset: `T${i}-${UNPRICED.split('-')[1]}`,
      balance: '10000000',
    }));
    stubApi({
      trustlines: [{ asset: USDC, balance: USDC_BALANCE }, ...filler],
      batch: strictBatch(),
    });
    renderPanel();
    // 121 rows is the heaviest render here; give the poll headroom.
    expect(
      await screen.findAllByText('≥ $69.50', {}, { timeout: 5000 }),
    ).toHaveLength(2);
    shows('2 priced');
    const calls = batchCalls();
    expect(calls).toHaveLength(2);
    for (const ids of calls) expect(ids.length).toBeLessThanOrEqual(100);
    expect(calls.flat()).toHaveLength(121);
  }, 10_000);

  it('says so when the price lookup itself failed', async () => {
    stubApi({ batch: strictBatch(true) });
    renderPanel();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Price lookup failed for 2 of 2 holdings',
    );
  });
});
