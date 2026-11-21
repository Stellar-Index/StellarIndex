import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';

import { ChartPanel, chartQuoteFor, USDC_ASSET_ID } from './ChartPanel';

describe('ChartPanel for USDC', () => {
  it('never prints a constant dollar price that would hide a depeg', () => {
    const { container } = render(<ChartPanel assetID={USDC_ASSET_ID} />);
    expect(container.textContent).not.toMatch(/\$1\.00/);
    expect(container.textContent).toMatch(/depeg/);
  });
});

// Operator decision: every price chart is anchored to USDC,
// never XLM (no quote toggle). USDC itself charts against fiat:USD so
// a depeg is visible instead of a definitional flat 1.0.
describe('chartQuoteFor', () => {
  it('quotes ordinary assets in USDC', () => {
    const q = chartQuoteFor(
      'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA',
    );
    expect(q.quote).toBe(USDC_ASSET_ID);
    expect(q.label).toBe('USDC');
  });

  it('quotes USDC itself in fiat USD (depeg surface)', () => {
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
