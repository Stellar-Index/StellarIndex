import { describe, it, expect, vi, beforeEach } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetEntryChangesPanel } from './AssetEntryChangesPanel';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const HOLDER = 'GDSQAEHJLE2ZZMQZ47YWLP3O2HVPYQ4QCFWTHUKMKF6RIX2ZJJDDMK4N';

function page(next?: string) {
  return {
    data: {
      asset: USDC,
      changes: [
        {
          ledger: 64802920,
          ledger_close_time: '2026-10-18T03:00:00Z',
          intra_ledger_seq: 4,
          tx_hash: 'ab'.repeat(32),
          op_index: 0,
          change_index: 1,
          role: 'holder',
          entry_type: 'trustline',
          change_type: 'updated',
          changed: ['balance'],
          account: HOLDER,
          amount: '1000000000000',
          entry: {},
        },
      ],
      next_cursor: next,
      through_ledger: 64802921,
      lower_bound: true,
      coverage_note: 'decoded through ledger 64802921',
    },
  };
}

function renderPanel(assetID: string) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetEntryChangesPanel assetID={assetID} />
    </QueryClientProvider>,
  );
}

describe('AssetEntryChangesPanel', () => {
  beforeEach(() => vi.mocked(apiGet).mockReset());

  it('lists a trustline change with its balance and coverage note', async () => {
    vi.mocked(apiGet).mockResolvedValue(page());
    renderPanel(USDC);
    expect(await screen.findByText('Trustline')).toBeInTheDocument();
    expect(screen.getByText('100,000')).toBeInTheDocument();
    expect(screen.getByText('balance')).toBeInTheDocument();
    expect(
      screen.getByText('decoded through ledger 64802921'),
    ).toBeInTheDocument();
  });

  it('pages older history with the served cursor', async () => {
    vi.mocked(apiGet).mockResolvedValue(page('64802900.x.0.1.holder'));
    renderPanel(USDC);
    fireEvent.click(await screen.findByText('Load older'));
    await waitFor(() =>
      expect(vi.mocked(apiGet)).toHaveBeenLastCalledWith(
        `/v1/assets/${USDC}/entry-changes`,
        { cursor: '64802900.x.0.1.holder' },
      ),
    );
  });

  it('renders nothing for native XLM, which has no trustlines', () => {
    const { container } = renderPanel('native');
    expect(container).toBeEmptyDOMElement();
    expect(vi.mocked(apiGet)).not.toHaveBeenCalled();
  });
});
