import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { ConvertPair } from './ConvertPair';

// RLT-384. The converter is a static export: `initialRate` is baked at
// BUILD time (page.tsx generateStaticParams + next.config `output:
// 'export'`), so the fallback number can be days or weeks old. Three
// ways the widget used to misrepresent it, all caused by typing the
// /v1/price/batch response as `{data: Array<{asset_id, price}>}` and
// throwing the rest of the envelope away:
//
//  1. freshness was stamped from React Query's `dataUpdatedAt` — the
//     instant the FETCH resolved — so any number on screen, including
//     the baked one, read "Updated 0s ago";
//  2. `/v1/price/batch` OMITS a pair it will not price (withheld, or
//     never observed). That resolves as a SUCCESS with no row, the live
//     rate came back null, and the baked rate silently took its place
//     dressed as the current one;
//  3. `price_type: peg` — the operator's standing 1:1 declaration, not
//     an observation — rendered identically to an observed VWAP.
//
// Measured against the live API on 2026-09-19: fiat:EUR/fiat:USD came
// back `observed_at: 2026-09-18T00:00:00Z` with `flags.stale: true`,
// i.e. ~36h old, while the widget said "Updated 0s ago".

const BAKED_RATE = 0.85; // 1 USD = 0.85 EUR, baked at build
const BAKED_INVERSE = 1 / BAKED_RATE;

/**
 * An `observed_at` in the past. 36h is what the live FX feed actually
 * carried on 2026-09-19; formatRelative renders anything past a day in
 * days, so 36h reads "2d ago".
 */
function hoursAgo(h: number): string {
  return new Date(Date.now() - h * 3_600_000).toISOString();
}

function stubBatch(body: unknown) {
  vi.mocked(apiGet).mockResolvedValue(body);
}

function renderWidget() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ConvertPair
        from="USD"
        to="EUR"
        initialRate={BAKED_RATE}
        initialInverse={BAKED_INVERSE}
      />
    </QueryClientProvider>,
  );
}

describe('ConvertPair freshness', () => {
  it('stamps the rate from the row observed_at, not from the fetch time', async () => {
    // 1 EUR = 2 USD ⇒ 1 USD = 0.50 EUR, observed 36h ago.
    stubBatch({
      data: [
        {
          asset_id: 'fiat:EUR',
          quote: 'fiat:USD',
          price: '2',
          price_type: 'vwap',
          observed_at: hoursAgo(36),
        },
      ],
      as_of: new Date().toISOString(),
      flags: { stale: false },
    });
    renderWidget();

    expect(await screen.findByText(/Updated 2d ago/)).toBeInTheDocument();
    // The old stamp came off `dataUpdatedAt`, which is always "now".
    expect(screen.queryByText(/Updated 0s ago/)).not.toBeInTheDocument();
    // …and the rate itself is the live one (caption + result field).
    expect(screen.getAllByText('0.500000').length).toBeGreaterThan(0);
    expect(screen.queryByText('0.850000')).not.toBeInTheDocument();
  });

  it('carries the envelope stale flag onto the rate line', async () => {
    stubBatch({
      data: [
        {
          asset_id: 'fiat:EUR',
          quote: 'fiat:USD',
          price: '2',
          price_type: 'vwap',
          observed_at: hoursAgo(36),
        },
      ],
      as_of: new Date().toISOString(),
      flags: { stale: true },
    });
    renderWidget();

    expect(
      await screen.findByText(/Updated 2d ago · mid-market VWAP · stale/),
    ).toBeInTheDocument();
  });
});

describe('ConvertPair omitted row', () => {
  it('labels the baked rate as unavailable instead of passing it off as current', async () => {
    // The pair is withheld / never observed: the batch answers 200 with
    // the row OMITTED. This is the case that used to fall back silently.
    stubBatch({
      data: [],
      as_of: new Date().toISOString(),
      flags: { stale: false },
    });
    renderWidget();

    expect(
      await screen.findByText(
        /Rate unavailable — showing the last published rate/,
      ),
    ).toBeInTheDocument();
    // The baked number is still shown (it IS the last published rate) —
    // but never with a freshness stamp attached to it.
    expect(screen.getAllByText('0.850000').length).toBeGreaterThan(0);
    expect(screen.queryByText(/Updated/)).not.toBeInTheDocument();
  });
});

describe('ConvertPair declared basis', () => {
  it('labels a peg price as a declaration rather than an observed rate', async () => {
    stubBatch({
      data: [
        {
          asset_id: 'fiat:EUR',
          quote: 'fiat:USD',
          price: '1',
          price_type: 'peg',
          observed_at: hoursAgo(2),
        },
      ],
      as_of: new Date().toISOString(),
      flags: { stale: false },
    });
    renderWidget();

    expect(
      await screen.findByText(/declared 1:1 peg, not an observed market rate/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/mid-market VWAP/)).not.toBeInTheDocument();
  });
});
