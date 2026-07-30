import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountActivitySummaryPanel } from './AccountActivitySummary';
import { AccountTradesPanel } from './AccountTrades';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{ui}</QueryClientProvider>);
}

describe('AccountActivitySummaryPanel', () => {
  it('renders segmented counts and distinguishes a MISSING segment from zero', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        ops_by_type: [
          { op_type: 'payment', count: 120 },
          { op_type: 'manage_buy_offer', count: 14 },
        ],
        // trades_total ABSENT (segment could not be read) — must render
        // as "—", never as 0.
        defi_actions: [{ protocol: 'blend', action: 'supply', count: 3 }],
        bridge_transfers: {
          rozo_outbound_payments: 1,
          rozo_inbound_payments: 0,
          cctp_outbound_burns: 0,
          cctp_inbound_mints: 0,
          note: 'rozo matches payment from_addr (outbound) / destination (inbound).',
        },
        coverage_note: 'INCOMPLETE: trades_total could not be read.',
      },
    });

    renderWithClient(<AccountActivitySummaryPanel id={G} />);

    // Ops total = 120 + 14, and the per-type breakdown renders.
    await waitFor(() => expect(screen.getByText('134')).toBeInTheDocument());
    expect(screen.getByText('payment')).toBeInTheDocument();

    // The failed segment renders the coverage note and a dash, not zero.
    expect(screen.getByText(/INCOMPLETE: trades_total/)).toBeInTheDocument();
    expect(screen.getByText('Attributed trades').nextElementSibling?.textContent).toBe('—');

    // DeFi + bridge segments render their real counts.
    expect(screen.getByText('supply')).toBeInTheDocument();
    expect(screen.getByText(/rozo matches payment from_addr/)).toBeInTheDocument();
  });
});

describe('AccountTradesPanel', () => {
  it('renders trade rows with string amounts and the attribution-scope note', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: {
        account: G,
        trades: [
          {
            ts: '2026-07-29T12:00:00Z',
            source: 'sdex',
            base_asset: 'native',
            quote_asset: `USDC-${G}`,
            base_amount: '10000000',
            quote_amount: '1234567',
            usd_volume: '0.12345600',
            tx_hash: 'be8ac09cf011950987ae7c17badec336ccf24782a03f5573b1f982cb44c98f36',
            ledger: 63000001,
            op_index: 0,
            role: 'taker',
          },
        ],
        note: 'Soroswap swaps do not yet record the acting account.',
      },
    });

    renderWithClient(<AccountTradesPanel id={G} />);

    // Amounts render verbatim as decimal strings (never reformatted
    // through a float).
    await waitFor(() => expect(screen.getByText('10000000')).toBeInTheDocument());
    expect(screen.getByText('1234567')).toBeInTheDocument();
    expect(screen.getByText('$0.12345600')).toBeInTheDocument();
    expect(screen.getByText('taker')).toBeInTheDocument();
    expect(screen.getByText('sdex')).toBeInTheDocument();

    // The attribution-scope honesty note is always visible.
    expect(screen.getByText(/Soroswap swaps do not yet record/)).toBeInTheDocument();
  });

  it('renders an honest empty state (no attributed trades ≠ never traded)', async () => {
    vi.mocked(apiGet).mockResolvedValue({
      data: { account: G, trades: [], note: 'attribution scope note' },
    });

    renderWithClient(<AccountTradesPanel id={G} />);

    await waitFor(() =>
      expect(screen.getByText(/No attributed trades observed/)).toBeInTheDocument(),
    );
    expect(screen.getByText('attribution scope note')).toBeInTheDocument();
  });
});
