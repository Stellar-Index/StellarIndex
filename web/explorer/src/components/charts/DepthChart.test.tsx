import { describe, it, expect } from 'vitest';
import { render, screen } from '@testing-library/react';

import {
  DepthChart,
  OrderBookStatStrip,
  computeBookStats,
  formatDepthPrice,
  type DepthLevel,
} from './DepthChart';

function level(price: string, cumBase: string, cumQuote = '0'): DepthLevel {
  return { price, cum_base_amount: cumBase, cum_quote_amount: cumQuote };
}

describe('computeBookStats', () => {
  it('derives spread and mid from the top of each side', () => {
    const stats = computeBookStats(
      [level('0.4900000', '10'), level('0.4800000', '30')],
      [level('0.5000000', '5'), level('0.5100000', '25')],
    );
    expect(stats.bestBid).toBeCloseTo(0.49);
    expect(stats.bestAsk).toBeCloseTo(0.5);
    expect(stats.spread).toBeCloseTo(0.01);
    expect(stats.mid).toBeCloseTo(0.495);
    expect(stats.spreadBps).toBeCloseTo((0.01 / 0.495) * 10_000, 3);
    expect(stats.crossed).toBe(false);
  });

  it('flags a crossed snapshot instead of serving a negative spread as fact', () => {
    const stats = computeBookStats([level('0.60', '10')], [level('0.50', '5')]);
    expect(stats.crossed).toBe(true);
    expect(stats.spread).toBeCloseTo(-0.1);
  });

  it('yields nulls (never zeros) when a side is missing', () => {
    const stats = computeBookStats([], [level('0.50', '5')]);
    expect(stats.bestBid).toBeNull();
    expect(stats.spread).toBeNull();
    expect(stats.mid).toBeNull();
    expect(stats.crossed).toBe(false);
  });
});

describe('OrderBookStatStrip', () => {
  it('renders best bid/ask, mid and spread for a normal book', () => {
    render(
      <OrderBookStatStrip
        stats={computeBookStats([level('0.49', '10')], [level('0.50', '5')])}
        quoteLabel="USDC"
      />,
    );
    expect(screen.getByText('Best bid')).toBeInTheDocument();
    expect(screen.getByText('Best ask')).toBeInTheDocument();
    expect(screen.getByText('Mid (USDC)')).toBeInTheDocument();
    expect(screen.getByText(/bps/)).toBeInTheDocument();
    expect(screen.queryByText(/crossed/i)).not.toBeInTheDocument();
  });

  it('replaces mid/spread with an explicit warning on a crossed book', () => {
    render(
      <OrderBookStatStrip
        stats={computeBookStats([level('0.60', '10')], [level('0.50', '5')])}
        quoteLabel="USDC"
      />,
    );
    expect(screen.getByText(/crossed/i)).toBeInTheDocument();
    expect(screen.queryByText(/bps/)).not.toBeInTheDocument();
  });

  it('renders — for missing sides', () => {
    render(
      <OrderBookStatStrip stats={computeBookStats([], [])} quoteLabel="USDC" />,
    );
    expect(screen.getAllByText('—').length).toBe(4);
  });
});

describe('DepthChart', () => {
  it('renders a labelled mirrored depth chart', () => {
    render(
      <DepthChart
        bids={[level('0.49', '10', '4.9'), level('0.48', '30', '14.5')]}
        asks={[level('0.50', '5', '2.5'), level('0.51', '25', '12.7')]}
        baseLabel="XLM"
        quoteLabel="USDC"
      />,
    );
    expect(screen.getByText('Bids')).toBeInTheDocument();
    expect(screen.getByText('Asks')).toBeInTheDocument();
    expect(
      screen.getByRole('img', {
        name: /2 bid levels and 2 ask levels of XLM priced in USDC/,
      }),
    ).toBeInTheDocument();
  });

  it('renders nothing when neither side has a plottable level', () => {
    const { container } = render(
      <DepthChart bids={[]} asks={[]} baseLabel="XLM" quoteLabel="USDC" />,
    );
    expect(container.firstChild).toBeNull();
  });
});

describe('formatDepthPrice', () => {
  it('adapts precision across magnitudes and never fabricates a value', () => {
    expect(formatDepthPrice(1234.5678)).toBe('1234.57');
    expect(formatDepthPrice(1.23456)).toBe('1.2346');
    expect(formatDepthPrice(0.168218)).toBe('0.168218');
    // F-A4-03: plain decimal below 1e-4 (2026-08-06 no-scientific-notation rule)
    expect(formatDepthPrice(0.00001)).toBe('0.00001');
    expect(formatDepthPrice(Number.NaN)).toBe('—');
  });
});
