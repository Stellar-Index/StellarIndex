import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

// The /assets directory rendered a wall of em-dashes above its priced rows.
//
// The listing's server-side ORDER BY leads with a rank tier that demotes an
// unpriced asset, but that tier reads the price rollup while the handler
// withholds the price of any row whose backing pairs fail the substance
// floor — a per-pair verdict measured at request time, invisible to the
// query that ranked the page. So rows rank as priced and serve as dashes.
//
// Measured against api.stellarindex.io 2026-09-03 21:31 UTC,
// `/v1/assets?asset_class=all&limit=100&include=sparkline7d`: 78 rows, 33
// with price_usd, first unpriced row at position 16 (EURZ) above priced
// USDV, BTC and sUSD, with eighteen priced rows trailing it — TESOURO at 43
// and USTRY at 45 below a run of nine consecutive dashes (34–42).
//
// The fixture reproduces that interleave in miniature, in server order.

const ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';

function coin(
  code: string,
  fields: Partial<Coin> & { volume_24h_usd: string },
): Coin {
  return {
    kind: 'stellar_asset',
    asset_id: `${code}-${ISSUER}`,
    code,
    slug: `${code}-${ISSUER}`,
    issuer: ISSUER,
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    ...fields,
  } as unknown as Coin;
}

// Every row carries a circulating supply so the "Circulating" case below
// ranks on the values alone, EURZ (the unpriced row) largest.
const assets: Coin[] = [
  coin('USDZ', {
    price_usd: '1.0010792014',
    volume_24h_usd: '52422.98',
    circulating_supply: '10000000000',
  }),
  // Unpriced, yet ranked above the two priced rows that trail it.
  coin('EURZ', {
    volume_24h_usd: '51556.15',
    circulating_supply: '90000000000000000000',
  }),
  coin('XRP', {
    price_usd: '1.4631046310',
    volume_24h_usd: '68784.30',
    circulating_supply: '20000000000',
  }),
  coin('APPLELEGACY', {
    volume_24h_usd: '2694.96',
    circulating_supply: '30000000000',
  }),
  coin('TESOURO', {
    price_usd: '0.2449733656',
    volume_24h_usd: '1180.52',
    circulating_supply: '40000000000',
  }),
];

const PRICED_FIRST = ['USDZ', 'XRP', 'TESOURO', 'EURZ', 'APPLELEGACY'];

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAssets: () => ({
      data: { assets, next_cursor: '' },
      isLoading: false,
      isError: false,
      error: null,
    }),
  };
});

function renderTable(props: Parameters<typeof AssetsTable>[0] = {}) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable {...props} />
    </QueryClientProvider>,
  );
}

// renderedCodes reads the asset code out of each row's Asset cell, in DOM
// order. The cell also carries the middle-truncated raw id, so match on the
// leading code rather than the whole text.
function renderedCodes(): string[] {
  return screen
    .getAllByRole('row')
    .slice(1) // header
    .map((tr) => {
      const label = tr.querySelectorAll('td')[1]?.textContent?.trim() ?? '';
      return (
        assets.find((a) => label.startsWith(String(a.code)))?.code ?? label
      );
    });
}

describe('AssetsTable priced-first default ranking', () => {
  it('ranks every row with a served price above every row without one', () => {
    renderTable();
    expect(renderedCodes()).toEqual(PRICED_FIRST);
  });

  it('demotes without hiding — every fetched row is still on the page', () => {
    renderTable();
    // Same rows, same count: this is a re-ranking, not a filter. An asset the
    // substance gate cannot price is still an asset the directory lists.
    expect(renderedCodes()).toHaveLength(assets.length);
    for (const a of assets) {
      expect(renderedCodes()).toContain(a.code);
    }
  });

  it('leaves an explicit column sort alone', () => {
    renderTable();
    fireEvent.click(screen.getByRole('button', { name: /Circulating/ }));
    // EURZ has no price and the largest circulating supply. The user asked
    // for a supply ranking, so it leads — useTableSort already sinks that
    // column's own blanks, and overriding the request would make the header
    // useless on a directory whose long tail is mostly unpriced.
    expect(renderedCodes()[0]).toBe('EURZ');
  });
});

// The footer states the rule the table applies. The substance-floor
// explanation is only true of the Stellar listing: /external/assets shares
// this component (endpoint=/v1/external/assets), and its fiat and reference
// coins are outside the substance gate's scope entirely — an unpriced row
// there (POL, AAVE and WBTC on 2026-09-03) is one the reference feed does
// not cover. That page must state the rule without asserting a cause that
// is false for every row it lists.
describe('AssetsTable footer note on unpriced rows', () => {
  it('names the substance floor on the Stellar listing', () => {
    renderTable();
    expect(screen.getByText(/rank below the priced ones/)).toHaveTextContent(
      /substance floor/,
    );
  });

  it('states the rule without the on-chain cause on /external/assets', () => {
    renderTable({
      endpoint: '/v1/external/assets',
      basePath: '/external/assets',
    });
    expect(screen.queryByText(/substance floor/)).toBeNull();
    expect(screen.getByText(/rank below the priced ones/)).toBeInTheDocument();
    // The ranking itself still applies there: it partitions on the price the
    // response carried, whatever the reason a row has none.
    expect(renderedCodes()).toEqual(PRICED_FIRST);
  });
});
