import { afterEach, describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { resolveScaleRefused } from './priceProvenance';

import { ChartPanel, chartQuoteFor, USDC_ASSET_ID } from './ChartPanel';

// /v1/ohlc and the tip stream never check a token's read decimals.
describe('ChartPanel under a contradicted scale', () => {
  afterEach(() => vi.unstubAllGlobals());

  function renderChart(row: Record<string, unknown> | null) {
    vi.stubGlobal(
      'fetch',
      vi.fn(() => new Promise(() => {})),
    );
    const qc = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    // The live row, as useAsset caches it.
    if (row) qc.setQueryData(['/v1/assets/{id}', 'CTOKEN'], { data: row });
    return render(
      <QueryClientProvider client={qc}>
        <ChartPanel assetID="CTOKEN" scaleMismatch={row == null} />
      </QueryClientProvider>,
    );
  }

  it('withholds the price chart on the build-time verdict', () => {
    renderChart(null);
    expect(screen.getByText(/Price chart withheld/)).toBeInTheDocument();
    expect(screen.queryByText(/OHLC/)).toBeNull();
  });

  it('withholds the price chart when the live row refuses the scale', async () => {
    renderChart({
      asset_id: 'CTOKEN',
      price_withheld_reason: 'decimals_mismatch',
    });
    expect(screen.getByText(/Price chart withheld/)).toBeInTheDocument();
  });
});

describe('ChartPanel for USDC', () => {
  it('never prints a constant dollar price that would hide a depeg', () => {
    const { container } = render(
      <ChartPanel assetID={USDC_ASSET_ID} scaleMismatch={false} />,
    );
    expect(container.textContent).not.toMatch(/\$1\.00/);
    expect(container.textContent).toMatch(/depeg/);
  });
});

describe('resolveScaleRefused', () => {
  it('keeps a build-time refusal when the live row could not read decimals', () => {
    expect(resolveScaleRefused({ price_usd: '1' }, true)).toBe(true);
  });

  it('lets a live row that read its decimals clear the build-time refusal', () => {
    expect(resolveScaleRefused({ price_usd: '1', decimals: 7 }, true)).toBe(
      false,
    );
  });
});

// Operator decision: every price chart is anchored to USDC,
// never XLM (no quote toggle). USDC itself maps to fiat:USD rather than a
// definitional flat USDC/USDC; ChartPanel shows it a reference panel.
describe('chartQuoteFor', () => {
  it('quotes ordinary assets in USDC', () => {
    const q = chartQuoteFor(
      'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA',
    );
    expect(q.quote).toBe(USDC_ASSET_ID);
    expect(q.label).toBe('USDC');
  });

  it('quotes USDC itself in fiat USD, never USDC/USDC', () => {
    const q = chartQuoteFor(USDC_ASSET_ID);
    expect(q.quote).toBe('fiat:USD');
  });

  it('quotes native XLM in fiat USD', () => {
    expect(chartQuoteFor('native').quote).toBe('fiat:USD');
  });

  it('quotes fiat assets in fiat USD', () => {
    expect(chartQuoteFor('fiat:EUR').quote).toBe('fiat:USD');
  });

  it('quotes off-chain crypto in fiat USD', () => {
    expect(chartQuoteFor('crypto:BTC').quote).toBe('fiat:USD');
  });

  it('never quotes anything in XLM', () => {
    for (const id of [
      'native',
      USDC_ASSET_ID,
      'yXLM-GARDNV3Q7YGT4AKSDF25LT32YSCCW4EV22Y2TV3I2PU2MMXJTEDL5T55',
      'fiat:GBP',
      'CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32',
    ]) {
      expect(chartQuoteFor(id).quote).not.toBe('native');
    }
  });
});
