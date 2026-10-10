import { afterEach, beforeEach, describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

const SCAM_ISSUER = 'GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK';
const SCAM_ID = `JFKBANK2-${SCAM_ISSUER}`;
const USDV_ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';
const SOROBAN_ID = 'CAUPHTOPWQOTVWZNTFPTSY6TIWU2LGZQVLGCEB2NF4YGKS6XUSGHIPXV';
const EXTERNAL = {
  endpoint: '/v1/external/assets',
  basePath: '/external/assets',
};

function coin(
  code: string | undefined,
  issuer: string | undefined,
  fields: Partial<Coin> & { volume_24h_usd: string },
): Coin {
  const id = issuer ? `${code}-${issuer}` : (fields.asset_id ?? 'x');
  return {
    kind: 'stellar_asset',
    asset_id: id,
    code,
    slug: id,
    issuer,
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
    ...fields,
  } as unknown as Coin;
}

// The mocked hook reads these; searchParams is mutable so a test can put the
// table on a cursor page (a hardcoded '' would run every test on page 1).
const state = vi.hoisted(() => ({
  assets: [] as Array<{ code?: string }>,
  searchParams: new URLSearchParams(''),
  thinOpts: [] as Array<{ includeThin?: boolean } | undefined>,
}));

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => state.searchParams,
}));

vi.mock('@/api/hooks', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/hooks')>('@/api/hooks');
  return {
    ...actual,
    useAssets: (...args: unknown[]) => {
      state.thinOpts.push(args[4] as { includeThin?: boolean } | undefined);
      return {
        data: { assets: state.assets, next_cursor: '' },
        isLoading: false,
        isError: false,
        error: null,
      };
    },
  };
});

afterEach(() => {
  state.searchParams = new URLSearchParams('');
  state.thinOpts.length = 0;
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

// First data cell index 0 is the rank, 1 is the Asset label (code plus the
// middle-truncated raw id).
const column = (i: number): string[] =>
  screen
    .getAllByRole('row')
    .slice(1) // header
    .map((tr) => tr.querySelectorAll('td')[i]?.textContent?.trim() ?? '');
const rankCells = () => column(0);
const labels = () => column(1);
const codes = (assets: Array<{ code?: string }>): string[] =>
  labels().map(
    (label) =>
      assets.find((a) => a.code && label.startsWith(a.code))?.code ?? label,
  );

// A flagged high-volume asset arrives above the legitimate ones; the scam
// gate withholds its numbers, so the ordering must follow.
describe('AssetsTable flagged-asset ranking', () => {
  beforeEach(() => {
    state.assets = [
      coin('JFKBANK2', SCAM_ISSUER, {
        volume_24h_usd: '62341.98',
        circulating_supply: '5000000000000000000',
        issuer_directory_tags: ['malicious', 'unsafe'],
      }),
      coin('USDV', USDV_ISSUER, {
        price_usd: '1.0001',
        volume_24h_usd: '1200.5',
      }),
      coin(undefined, undefined, {
        asset_id: SOROBAN_ID,
        price_usd: '0.02',
        volume_24h_usd: '900',
      }),
    ];
  });

  it('ranks the flagged asset below every unflagged one on the default order', () => {
    renderTable();
    const l = labels();
    const flagged = l.findIndex((x) => x.startsWith('JFKBANK2'));
    const unflagged = l.findIndex((x) => x.startsWith('USDV'));
    expect(flagged).toBeGreaterThan(-1);
    expect(unflagged).toBeGreaterThan(-1);
    expect(flagged).toBeGreaterThan(unflagged);
    // Demoted, never hidden: the row and its pill are still on the page.
    expect(screen.getAllByText(/Flagged/)).toHaveLength(1);
    expect(l).toHaveLength(3);
  });

  it('keeps the flagged asset last after the user sorts by 24h volume', () => {
    renderTable();
    // Volume desc would put JFKBANK2 ($62.3k) on top of USDV ($1.2k).
    fireEvent.click(screen.getByRole('button', { name: /Volume 24h/ }));
    const l = labels();
    expect(l.findIndex((x) => x.startsWith('JFKBANK2'))).toBeGreaterThan(
      l.findIndex((x) => x.startsWith('USDV')),
    );
  });

  it('middle-truncates the classic id, keeping head and tail, with the full value on hover and on copy', () => {
    renderTable();
    expect(screen.queryByText(SCAM_ID)).toBeNull();
    // Issuer strkeys differ near the end, so a head-only truncation is ambiguous.
    const elided = screen.getByText('JFKBANK2…L5BANK');
    expect(elided).toHaveAttribute('title', SCAM_ID);
    const row = elided.closest('tr') as HTMLElement;
    expect(
      row.querySelector('button[aria-label="Copy to clipboard"]'),
    ).not.toBeNull();
  });

  it('labels an uncatalogued Soroban row with its truncated contract id instead of an empty cell', () => {
    renderTable();
    const elided = screen.getByText('CAUPHT…IPXV');
    expect(elided).toHaveAttribute('title', SOROBAN_ID);
    expect(screen.queryByText(SOROBAN_ID)).toBeNull();
  });

  // The "#" column is a per-page counter and cursor pagination keeps no depth
  // in the URL, so page 2 would re-label the 101st asset "#1". Suppress rather
  // than guess: pages under-fill after post-query twin folding.
  it('numbers the rows on the unpaginated first page', () => {
    renderTable();
    expect(rankCells()).toEqual(['1', '2', '3']);
  });

  it('renders no rank once the user has paged past the first', () => {
    state.searchParams = new URLSearchParams('cursor=opaque-page-2-cursor');
    renderTable();
    expect(rankCells().every((c) => c === '')).toBe(true);
  });

  // A header re-sort is page-local, so numbering it would claim a directory-wide rank.
  it('renders no rank once the user sorts by a column', () => {
    renderTable();
    fireEvent.click(screen.getByRole('button', { name: /Volume 24h/ }));
    expect(rankCells().every((c) => c === '')).toBe(true);
  });
});

// The server's rank tier reads the price rollup, but the handler withholds the
// price of rows failing the substance floor, so rows rank as priced and serve
// as dashes. The fixture reproduces that interleave in server order.
describe('AssetsTable priced-first default ranking', () => {
  const PRICED_FIRST = [
    'USDZ',
    'XRP',
    'TESOURO',
    'THIN',
    'EURZ',
    'APPLELEGACY',
  ];
  // Every row carries a circulating supply so the "Circulating" sort ranks on
  // values alone, EURZ (unpriced) largest.
  const fixture = [
    coin('USDZ', USDV_ISSUER, {
      price_usd: '1.0010792014',
      volume_24h_usd: '52422.98',
      circulating_supply: '10000000000',
    }),
    // A thin-market price is served under include_thin but ranks unpriced.
    coin('THIN', USDV_ISSUER, {
      price_usd: '0.0500000000',
      thin_market: true,
      volume_24h_usd: '60000.00',
      circulating_supply: '50000000000',
    }),
    coin('EURZ', USDV_ISSUER, {
      volume_24h_usd: '51556.15',
      circulating_supply: '90000000000000000000',
    }),
    coin('XRP', USDV_ISSUER, {
      price_usd: '1.4631046310',
      volume_24h_usd: '68784.30',
      circulating_supply: '20000000000',
    }),
    coin('APPLELEGACY', USDV_ISSUER, {
      volume_24h_usd: '2694.96',
      circulating_supply: '30000000000',
    }),
    coin('TESOURO', USDV_ISSUER, {
      price_usd: '0.2449733656',
      volume_24h_usd: '1180.52',
      circulating_supply: '40000000000',
    }),
  ];
  beforeEach(() => {
    state.assets = fixture;
  });

  it('ranks every row with a served price above every row without one', () => {
    renderTable();
    expect(codes(fixture)).toEqual(PRICED_FIRST);
  });

  it('demotes without hiding — every fetched row is still on the page', () => {
    renderTable();
    const rendered = codes(fixture);
    expect(rendered).toHaveLength(fixture.length);
    for (const a of fixture) expect(rendered).toContain(a.code);
  });

  it('shows a thin-market price with a warning badge, ranked with the unpriced rows', () => {
    renderTable();
    expect(state.thinOpts.at(-1)?.includeThin).toBe(true);
    const row = screen.getAllByRole('row')[4];
    expect(row).toHaveTextContent('THIN');
    expect(row).toHaveTextContent('$0.05');
    expect(row?.querySelector('[title^="Low confidence"]')).not.toBeNull();
  });

  it('does not ask /external/assets for thin prices', () => {
    renderTable(EXTERNAL);
    expect(state.thinOpts.at(-1)?.includeThin).toBe(false);
  });

  it('leaves an explicit column sort alone', () => {
    renderTable();
    fireEvent.click(screen.getByRole('button', { name: /Circulating/ }));
    // EURZ is unpriced with the largest supply; the user asked for a supply
    // ranking, so it leads.
    expect(codes(fixture)[0]).toBe('EURZ');
  });

  // The substance-floor explanation is only true of the Stellar listing;
  // /external/assets shares the component but is outside that gate's scope.
  it('names the substance floor on the Stellar listing', () => {
    renderTable();
    expect(screen.getByTitle(/rank below priced ones/)).toHaveAttribute(
      'title',
      expect.stringMatching(/substance floor/),
    );
  });

  it('states the rule without the on-chain cause on /external/assets', () => {
    renderTable(EXTERNAL);
    const note = screen.getByTitle(/rank below priced ones/);
    expect(note.title).not.toMatch(/substance floor/);
    // The ranking still partitions on the price the response carried.
    expect(codes(fixture)).toEqual(PRICED_FIRST);
  });
});
