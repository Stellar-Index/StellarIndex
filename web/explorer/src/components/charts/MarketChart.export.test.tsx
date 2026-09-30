import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { afterEach, describe, expect, it, vi } from 'vitest';

import type { components } from '@/api/types';

import { MarketChart, ohlcCsv, ohlcExportName } from './MarketChart';

type OHLCBar = components['schemas']['OHLCSeriesBar'];

vi.mock('@/lib/live/hooks', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/lib/live/hooks')>()),
  useLedgerFollow: () => undefined,
  useTipStream: () => null,
  useLiveClock: () => Date.now(),
}));

// Values a double cannot hold: the export must carry the served strings.
const bar: OHLCBar = {
  t: '2026-07-03T20:00:00Z',
  o: '0.0000001234567890',
  h: '0.0000001234567891',
  l: '0.0000001234567889',
  c: '0.0000001234567890',
  v_base: '123456789012345678901234',
  v_quote: '15241578753238836.0000042405',
  v_base_decimals: 7,
  v_quote_decimals: null,
  n: 42,
};

afterEach(() => vi.unstubAllGlobals());

describe('MarketChart export', () => {
  it('writes every served value verbatim to CSV', () => {
    expect(ohlcCsv([bar])).toBe(
      't,o,h,l,c,v_base,v_quote,v_base_decimals,v_quote_decimals,n\r\n' +
        '2026-07-03T20:00:00Z,0.0000001234567890,0.0000001234567891,' +
        '0.0000001234567889,0.0000001234567890,123456789012345678901234,' +
        '15241578753238836.0000042405,7,,42\r\n',
    );
  });

  it('names the file after the pair, interval and span without path-hostile characters', () => {
    expect(
      ohlcExportName(
        'USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN',
        'native',
        '1h',
        [bar, { ...bar, t: '2026-07-03T21:00:00Z' }],
        'csv',
      ),
    ).toBe(
      'stellarindex-ohlc-USDC_GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN-native-1h-20260703T200000Z-20260703T210000Z.csv',
    );
  });

  it('offers CSV and JSON download once bars load', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn().mockResolvedValue({
        ok: true,
        status: 200,
        json: async () => ({ data: { intervals: [bar] } }),
      }),
    );
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(
      <QueryClientProvider client={client}>
        <MarketChart
          base="native"
          quote="fiat:USD"
          baseLabel="XLM"
          quoteLabel="USD"
        />
      </QueryClientProvider>,
    );
    expect(
      await screen.findByRole('group', { name: 'Download chart data' }),
    ).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'CSV' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'JSON' })).toBeInTheDocument();
  });
});
