import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

import { apiGet } from '@/api/client';
import { HomeCurrencies } from './HomeCurrencies';

// The strip's caption promised "full ~200-ticker coverage at /assets",
// which was wrong on both halves: the reference catalogue served by
// /v1/external/assets holds 34 rows (19 fiat + 15 coins), and /assets is
// the Stellar-only directory — it carries no fiat at all and points the
// reader back out to /external/assets. The caption must send them
// straight to the page that holds the rows.
function renderStrip() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <HomeCurrencies />
    </QueryClientProvider>,
  );
}

describe('HomeCurrencies caption', () => {
  it('does not promise ~200-ticker coverage', () => {
    renderStrip();
    expect(screen.queryByText(/200-ticker/i)).not.toBeInTheDocument();
  });

  it('links the reference set to /external/assets, not /assets', () => {
    renderStrip();
    const link = screen.getByRole('link', { name: '/external/assets' });
    expect(link).toHaveAttribute('href', '/external/assets');
  });

  it('states the real size of the reference set', () => {
    renderStrip();
    expect(
      screen.getByText(/19 fiat plus 15 reference coins/i),
    ).toBeInTheDocument();
  });
});

// RLT-384. The strip typed /v1/price/batch as `{data: Array<{asset_id,
// price}>}` and threw the envelope away, so it captioned itself "Live"
// over rates the API had flagged stale, showed a declared peg exactly
// like an observed FX quote, and left the 24h chip with no producer at
// all. Measured against the live API on 2026-09-19: the fiat rows came
// back `observed_at: 2026-09-18T00:00:00Z` with `flags.stale: true`.
function stubEnvelope() {
  vi.mocked(apiGet).mockResolvedValue({
    data: [
      {
        asset_id: 'fiat:EUR',
        quote: 'fiat:USD',
        price: '1.148765077541643',
        price_type: 'vwap',
        // 36h back — formatRelative renders anything past a day in days.
        observed_at: new Date(Date.now() - 36 * 3_600_000).toISOString(),
        change_24h_pct: '-0.42',
      },
      {
        asset_id: 'fiat:GBP',
        quote: 'fiat:USD',
        price: '1.339764201500536',
        price_type: 'peg',
        observed_at: new Date(Date.now() - 2 * 3_600_000).toISOString(),
      },
    ],
    as_of: new Date().toISOString(),
    sources: ['massive'],
    flags: { stale: true },
  });
}

describe('HomeCurrencies price envelope', () => {
  it('stamps the strip with the oldest observed_at and repeats the stale flag', async () => {
    stubEnvelope();
    renderStrip();
    expect(
      await screen.findByText(
        /Rates observed 2d ago · flagged stale by the pricing API/,
      ),
    ).toBeInTheDocument();
  });

  it('does not call the rates live', async () => {
    stubEnvelope();
    renderStrip();
    await screen.findByText(/Rates observed/);
    expect(screen.queryByText(/Live USD-base rates/)).not.toBeInTheDocument();
  });

  it('labels a declared peg instead of showing it as an observed rate', async () => {
    stubEnvelope();
    renderStrip();
    expect(await screen.findByText('declared peg')).toBeInTheDocument();
    // …and only on the peg row: EUR is an observed VWAP.
    expect(screen.getAllByText('declared peg')).toHaveLength(1);
  });

  it('renders the 24h chip from the row change_24h_pct', async () => {
    stubEnvelope();
    renderStrip();
    expect(await screen.findByText('-0.42%')).toBeInTheDocument();
  });
});
