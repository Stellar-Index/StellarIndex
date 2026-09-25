import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { HomeTopAssets } from './HomeTopAssets';
import { HomeTopMovers } from './HomeTopMovers';

// The directory-flag badge and the flagged-last ranking must hold on every
// home panel that renders /v1/assets rows, not only on /assets (#773).

const ISSUER = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const FLAGGED_ISSUER =
  'GBFLAGGEDISSUERXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXXX';

function row(code: string, change: string, volume: string, flagged = false) {
  const issuer = flagged ? FLAGGED_ISSUER : ISSUER;
  return {
    kind: 'stellar_asset',
    asset_id: `${code}-${issuer}`,
    type: 'classic',
    code,
    issuer,
    slug: `${code}-${issuer}`,
    decimals: 7,
    sep1_status: 'ok',
    price_usd: '1.00',
    change_24h_pct: change,
    volume_24h_usd: volume,
    observation_count: 100,
    ...(flagged ? { issuer_directory_tags: ['hack'] } : {}),
  };
}

function mockAssets(rows: unknown[]) {
  vi.mocked(apiGet).mockImplementation(async (path: string) => {
    if (path.startsWith('/v1/assets/verified')) return { data: [] };
    if (path.startsWith('/v1/assets/native')) return { data: null };
    if (path.startsWith('/v1/assets')) return { data: rows };
    return { data: [] };
  });
}

function renderWithQuery(ui: React.ReactElement) {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false, gcTime: 0 } },
  });
  return render(<QueryClientProvider client={qc}>{ui}</QueryClientProvider>);
}

/** Codes in the order the Gainers column renders them. */
function gainerCodes(): string[] {
  const column = screen.getByText('Gainers').closest('div') as HTMLElement;
  return within(column)
    .getAllByRole('link')
    .map((a) => a.querySelector('span')?.textContent ?? '');
}

describe('home /v1/assets panels and directory-flagged issuers', () => {
  beforeEach(() => {
    vi.mocked(apiGet).mockReset();
  });

  it('HomeTopAssets badges a flagged row', async () => {
    mockAssets([row('GOOD', '1', '900'), row('SCAM', '1', '800', true)]);
    renderWithQuery(<HomeTopAssets />);
    await waitFor(() => {
      expect(screen.getAllByText('GOOD').length).toBeGreaterThan(0);
    });
    const badges = screen.getAllByText('⚠ Flagged');
    expect(badges).toHaveLength(1);
    expect(badges[0].closest('tr')?.textContent).toContain('SCAM');
  });

  it('HomeTopAssets keeps a flagged row below every unflagged row', async () => {
    // A page whose flagged row arrives above an unflagged one (a cached
    // page from before the server's rank tier, or any future re-order)
    // must still render flagged last.
    mockAssets([row('SCAM', '1', '950', true), row('GOOD', '1', '900')]);
    renderWithQuery(<HomeTopAssets />);
    await waitFor(() => {
      expect(screen.getAllByText('GOOD').length).toBeGreaterThan(0);
    });
    const body = screen.getAllByRole('row').map((r) => r.textContent ?? '');
    const good = body.findIndex((t) => t.includes('GOOD'));
    const scam = body.findIndex((t) => t.includes('SCAM'));
    expect(good).toBeGreaterThan(-1);
    expect(scam).toBeGreaterThan(good);
  });

  it('HomeTopMovers never ranks a flagged gainer above an unflagged one', async () => {
    // The client-side sort by change_24h_pct used to float a flagged row
    // with the largest move straight to Gainers #1, undoing the server's
    // flagged-last rank tier.
    mockAssets([
      row('A', '5', '1'),
      row('B', '4', '1'),
      row('SCAM', '90', '1', true),
      row('C', '3', '1'),
    ]);
    renderWithQuery(<HomeTopMovers />);
    await waitFor(() => {
      expect(gainerCodes().length).toBe(4);
    });
    expect(gainerCodes()).toEqual(['A', 'B', 'C', 'SCAM']);
    expect(screen.getAllByText('⚠ Flagged')).toHaveLength(1);
  });

  it('HomeTopMovers drops a flagged gainer from a full top five', async () => {
    mockAssets([
      row('SCAM', '90', '1', true),
      row('A', '6', '1'),
      row('B', '5', '1'),
      row('C', '4', '1'),
      row('D', '3', '1'),
      row('E', '2', '1'),
    ]);
    renderWithQuery(<HomeTopMovers />);
    await waitFor(() => {
      expect(gainerCodes().length).toBe(5);
    });
    expect(gainerCodes()).toEqual(['A', 'B', 'C', 'D', 'E']);
  });
});
