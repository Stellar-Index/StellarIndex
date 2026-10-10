import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountView } from './AccountView';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

// The view fans out to many sibling panels, each fetching via apiGet. Only the
// routed endpoints resolve; the rest stay pending in their loading state.
function mockApi(routes: Record<string, unknown>) {
  vi.mocked(apiGet).mockImplementation((path: string) => {
    for (const [suffix, data] of Object.entries(routes)) {
      if (path === suffix || path.endsWith(suffix)) {
        return Promise.resolve({ data });
      }
    }
    return new Promise(() => {});
  });
}

function mockOps(operations: unknown[], coverage_note?: string) {
  mockApi({
    '/operations': {
      account: G,
      operations,
      scope: 'all',
      ...(coverage_note ? { coverage_note } : {}),
    },
  });
}

const op = (hash: string, type: string, extra: object = {}) => ({
  tx_hash: hash.repeat(64),
  op_index: 0,
  type,
  fields: {},
  ...extra,
});
const ok = { transaction_successful: true, transaction_result: 'tx_success' };

function renderWithClient(ui: React.ReactElement) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>{ui}</QueryClientProvider>,
  );
}

describe('AccountView operations history — failed-tx transparency', () => {
  it('marks a failed op with its reason slug in red, an unknown op as muted, and renders the coverage_note banner — hiding nothing', async () => {
    mockOps(
      [
        op('a', 'payment', {
          transaction_successful: false,
          transaction_result: 'tx_failed',
          fields: { amount: '10000000', destination: G },
        }),
        // transaction_successful omitted -> unknown, never "success".
        op('b', 'change_trust'),
        op('c', 'manage_sell_offer', ok),
      ],
      'DEGRADED: parent-transaction outcome read failed for some operations.',
    );

    renderWithClient(<AccountView id={G} />);

    const failed = await screen.findByText('tx_failed');
    expect(failed).toHaveClass('bg-down-subtle');
    expect(failed).toHaveClass('text-down-strong');

    const unknown = screen.getByText('unknown');
    expect(unknown).toHaveClass('text-ink-muted');
    expect(unknown).toHaveAttribute('title', 'transaction outcome unavailable');
    expect(unknown).not.toHaveClass('bg-up-subtle');

    expect(screen.getByText('success')).toHaveClass('bg-up-subtle');

    expect(screen.getByRole('alert')).toHaveTextContent(
      'DEGRADED: parent-transaction outcome read failed',
    );

    // Every op stays listed, just marked.
    expect(screen.getByText('payment')).toBeInTheDocument();
    expect(screen.getByText('change_trust')).toBeInTheDocument();
    expect(screen.getByText('manage_sell_offer')).toBeInTheDocument();
  });

  it('does not render a coverage_note banner when every outcome is known', async () => {
    mockOps([op('d', 'payment', ok)]);

    renderWithClient(<AccountView id={G} />);

    await screen.findByText('payment');
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(screen.queryByText('unknown')).not.toBeInTheDocument();
  });
});

describe('AccountView history paging — empty page with next_cursor', () => {
  function mockEmptyPage(next_cursor?: string) {
    const cursor = next_cursor ? { next_cursor } : {};
    mockApi({
      '/operations': { account: G, operations: [], scope: 'all', ...cursor },
      '/transactions': { account: G, transactions: [], ...cursor },
    });
  }

  it('keeps the Older button and a neutral line when the page is empty but next_cursor is set', async () => {
    mockEmptyPage('cur-1');
    renderWithClient(<AccountView id={G} />);

    const notes = await screen.findAllByText(
      'No visible items on this page — older history continues.',
    );
    expect(notes).toHaveLength(2);
    expect(screen.queryByText(/observed for this account yet/)).toBeNull();
    const older = screen.getAllByRole('button', { name: /Load older/ });
    expect(older).toHaveLength(2);
    fireEvent.click(older[0]);
    expect(vi.mocked(apiGet)).toHaveBeenCalledWith(
      expect.stringContaining('/transactions'),
      expect.anything(),
    );
  });

  it('shows the empty state only when the page is empty and there is no next_cursor', async () => {
    mockEmptyPage();
    renderWithClient(<AccountView id={G} />);

    expect(
      await screen.findByText('No transactions observed for this account yet.'),
    ).toBeInTheDocument();
    expect(
      screen.getByText('No operations observed for this account yet.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /Load older/ })).toBeNull();
  });
});

describe('AccountView state charts', () => {
  it('bars each trustline’s limit used and each signer’s weight', async () => {
    mockApi({
      [`/v1/accounts/${G}`]: {
        account_id: G,
        exists: true,
        balance: '100000000',
        signers: [{ key: G, weight: 10 }],
        trustlines: [
          {
            asset: `USDC:${G}`,
            balance: '2500000000',
            limit: '10000000000',
            flags: 1,
          },
          {
            // An i64-max limit: the share is divided in BigInt, not as doubles.
            asset: `EURC:${G}`,
            balance: '4611686018427387904',
            limit: '9223372036854775807',
            flags: 1,
          },
        ],
      },
    });

    renderWithClient(<AccountView id={G} />);

    expect(await screen.findByText('25%')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: '25% of limit used' }),
    ).toBeInTheDocument();
    expect(screen.getByText('50%')).toBeInTheDocument();
    expect(
      screen.getByRole('img', { name: 'Weight 10 of 255' }),
    ).toBeInTheDocument();
  });
});

describe('AccountView non-G addresses', () => {
  it('links a muxed M-address to its base account', () => {
    renderWithClient(
      <AccountView id="MA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJUAAAAAAAAAAAACJUQ" />,
    );
    const link = screen.getByRole('link', {
      name: 'GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ',
    });
    expect(link.getAttribute('href')).toBe(
      '/accounts/GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ/',
    );
  });

  it('links a C-address to the contract page', () => {
    const C = 'CA' + 'A'.repeat(54);
    renderWithClient(<AccountView id={C} />);
    expect(
      screen.getByRole('link', { name: /open contract/i }).getAttribute('href'),
    ).toBe(`/contracts/${C}/`);
  });
});
