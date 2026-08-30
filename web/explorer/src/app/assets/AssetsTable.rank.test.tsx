import { afterEach, describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetsTable } from './AssetsTable';
import type { Coin } from '@/api/hooks';

// #356 — the /assets directory table.
//
// 1. RANKING. JFKBANK2 (issuer tagged malicious/unsafe) rendered at #12
//    on the live page — above USDV, MJQ and BRAVO — on $62.32K of 24h
//    volume with no price, no market cap and no %-changes. The scam gate
//    withheld its numbers; the ordering never followed. A flagged asset
//    must sit below every unflagged one whatever the active sort key,
//    with its row and its ⚠ Flagged pill intact.
// 2. IDENTIFIERS. Rows rendered raw 65-character ids in full. They must
//    be middle-truncated with the tail kept (issuer strkeys differ near
//    the end) and the full value reachable on hover and on copy.

const SCAM_ISSUER = 'GB7KFNUR5IAIN5NTYM2BUWWUTM6QMUBXF7NHXXKAMRPFLFWR7KL5BANK';
const SCAM_ID = `JFKBANK2-${SCAM_ISSUER}`;
const USDV_ISSUER = 'GBLTXF46JTCGMWFJASQLVXMMA36IPYTDCN4EN73HRXCGDCGYBZM3A6VD';
const USDV_ID = `USDV-${USDV_ISSUER}`;
const SOROBAN_ID = 'CAUPHTOPWQOTVWZNTFPTSY6TIWU2LGZQVLGCEB2NF4YGKS6XUSGHIPXV';

// baseCoin is the minimum shape the table needs; each row spreads its
// own fields over it.
function baseCoin(): Coin {
  return {
    kind: 'stellar_asset',
    asset_id: 'x',
    code: 'X',
    slug: 'x',
    decimals: 7,
    sep1_status: 'not_applicable',
    first_seen_ledger: 1,
    last_seen_ledger: 1,
    observation_count: 1,
  } as unknown as Coin;
}

// Incoming order mirrors the reported page: the flagged high-volume row
// arrives ABOVE the legitimate ones.
const assets: Coin[] = [
  {
    ...baseCoin(),
    asset_id: SCAM_ID,
    code: 'JFKBANK2',
    slug: SCAM_ID,
    issuer: SCAM_ISSUER,
    volume_24h_usd: '62341.98',
    circulating_supply: '5000000000000000000',
    issuer_directory_tags: ['malicious', 'unsafe'],
  } as unknown as Coin,
  {
    ...baseCoin(),
    asset_id: USDV_ID,
    code: 'USDV',
    slug: USDV_ID,
    issuer: USDV_ISSUER,
    price_usd: '1.0001',
    volume_24h_usd: '1200.5',
  } as unknown as Coin,
  {
    ...baseCoin(),
    asset_id: SOROBAN_ID,
    code: undefined,
    slug: SOROBAN_ID,
    price_usd: '0.02',
    volume_24h_usd: '900',
  } as unknown as Coin,
];

// Mutable so a test can put the table on a cursor-paginated page. The
// mock previously hardcoded '' — which is why nothing caught the rank
// column restarting at 1 on page 2 (EXR-06): every test ran on page 1.
let searchParams = new URLSearchParams('');

vi.mock('next/navigation', () => ({
  useRouter: () => ({ push: vi.fn() }),
  useSearchParams: () => searchParams,
}));

afterEach(() => {
  searchParams = new URLSearchParams('');
});

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

function renderTable() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetsTable />
    </QueryClientProvider>,
  );
}

// renderedCodes reads the first data cell of each row in DOM order.
function renderedRowLabels(): string[] {
  return screen
    .getAllByRole('row')
    .slice(1) // header
    .map((tr) => tr.querySelectorAll('td')[1]?.textContent?.trim() ?? '');
}

describe('AssetsTable flagged-asset ranking (#356)', () => {
  it('ranks the flagged asset below every unflagged one on the default order', () => {
    renderTable();
    const labels = renderedRowLabels();
    const flagged = labels.findIndex((l) => l.startsWith('JFKBANK2'));
    const unflagged = labels.findIndex((l) => l.startsWith('USDV'));
    expect(flagged).toBeGreaterThan(-1);
    expect(unflagged).toBeGreaterThan(-1);
    expect(flagged).toBeGreaterThan(unflagged);
    // Demoted, never hidden: the row and its pill are still on the page.
    expect(screen.getAllByText(/Flagged/)).toHaveLength(1);
    expect(labels).toHaveLength(3);
  });

  it('keeps the flagged asset last after the user sorts by 24h volume', () => {
    renderTable();
    // Volume desc would put JFKBANK2 ($62.3k) on top of USDV ($1.2k).
    fireEvent.click(screen.getByRole('button', { name: /Volume 24h/ }));
    const labels = renderedRowLabels();
    expect(labels.findIndex((l) => l.startsWith('JFKBANK2'))).toBeGreaterThan(
      labels.findIndex((l) => l.startsWith('USDV')),
    );
  });
});

describe('AssetsTable long-identifier truncation (#356)', () => {
  it('middle-truncates the classic id, keeping head and tail, with the full value on hover and on copy', () => {
    renderTable();
    // The raw 65-char id is never rendered in full...
    expect(screen.queryByText(SCAM_ID)).toBeNull();
    // ...the elided form keeps the head AND the tail (issuer strkeys
    // differ near the end, so a head-only truncation is ambiguous).
    const elided = screen.getByText('JFKBANK2…L5BANK');
    expect(elided).toBeInTheDocument();
    // The full value round-trips through the title attribute.
    expect(elided).toHaveAttribute('title', SCAM_ID);
    // ...and a copy control carries the un-elided value.
    const row = elided.closest('tr') as HTMLElement;
    expect(
      row.querySelector('button[aria-label="Copy to clipboard"]'),
    ).not.toBeNull();
  });

  it('labels an uncatalogued Soroban row with its truncated contract id instead of an empty cell', () => {
    renderTable();
    const elided = screen.getByText('CAUPHT…IPXV');
    expect(elided).toBeInTheDocument();
    expect(elided).toHaveAttribute('title', SOROBAN_ID);
    expect(screen.queryByText(SOROBAN_ID)).toBeNull();
  });
});

// The "#" column is a per-PAGE counter, but cursor pagination keeps no
// depth in the URL — only the opaque cursor. So on page 2 the counter
// restarted at 1 and re-labelled the 101st asset "#1" under a header
// that reads as a global rank (wave-D EXR-06).
//
// Suppression, not arithmetic: depth*limit+i would print a DIFFERENT
// wrong number, because suppressCatalogueTwins and foldAliasTwins drop
// rows post-query so pages under-fill (measured 81/96/99/96 at
// limit=100). A rank the data cannot back is better omitted than
// guessed.
describe('AssetsTable rank column across cursor pages (EXR-06)', () => {
  function rankCells(): string[] {
    return screen
      .getAllByRole('row')
      .slice(1) // header
      .map((tr) => tr.querySelectorAll('td')[0]?.textContent?.trim() ?? '');
  }

  it('numbers the rows on the unpaginated first page', () => {
    renderTable();
    expect(rankCells()[0]).toBe('1');
  });

  it('renders no rank once the user has paged past the first', () => {
    // Any non-empty cursor means "not page 1", which is all the URL
    // carries — there is no page number to recover.
    searchParams = new URLSearchParams('cursor=opaque-page-2-cursor');
    renderTable();
    expect(rankCells().every((c) => c === '')).toBe(true);
  });
});
