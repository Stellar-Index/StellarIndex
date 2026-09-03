// The /accounts header states the directory's ranking basis, and that
// basis is the SERVED `ranked_by` — the build's network flag is only the
// pre-load fallback (and what the static export bakes).
//
// The API ranks by native XLM whenever its price catalogue degrades to
// the lone native entry, a state mainnet can reach, and in it the Panel
// under the header is titled "Ranked by XLM balance" with every number
// formatted in XLM. A header pinned to CURRENT_NETWORK.pricing would say
// "ranked by the total USD value" directly above that table. Header and
// table subscribe to the same query, so they flip in the same render.
//
// The test env carries no NEXT_PUBLIC_NETWORK, so this is a MAINNET
// build (pricing: true) — the case where the fallback and the served
// basis disagree.
import { describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountsDirectoryBody, AccountsDirectoryHeader } from './AccountView';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

// Only the directory endpoint resolves; the analytics strip's own fetches
// stay pending so it sits in its harmless loading state.
function mockDirectory(ranked_by: 'usd' | 'native_xlm') {
  vi.mocked(apiGet).mockImplementation((path: string) => {
    if (path === '/v1/accounts') {
      return Promise.resolve({
        data: {
          priced_assets: ranked_by === 'usd' ? 12 : 0,
          ranked_by,
          accounts: [{ account_id: G, value: '123456' }],
        },
      });
    }
    return new Promise(() => {});
  });
}

function renderDirectory() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const { container } = render(
    <QueryClientProvider client={client}>
      <AccountsDirectoryHeader />
      <AccountsDirectoryBody />
    </QueryClientProvider>,
  );
  const header = container.querySelector('header');
  if (!header) throw new Error('directory header did not render');
  return header;
}

describe('AccountsDirectoryHeader — ranking basis follows the served ranked_by', () => {
  it('bakes the network fallback, then flips to the served basis with the table', async () => {
    mockDirectory('native_xlm');
    const header = renderDirectory();

    // Before the response: the mainnet fallback, which is what the static
    // export ships.
    expect(header.querySelector('h1')).toHaveTextContent('Accounts');
    expect(header).toHaveTextContent(/ranked by the total USD value/);

    // After it: the API ranked in XLM, so the header says so — above a
    // Panel that says the same, over XLM-formatted numbers.
    await waitFor(() =>
      expect(header).toHaveTextContent(/ranked by native XLM balance/),
    );
    expect(header).not.toHaveTextContent(/USD/);
    expect(
      screen.getByRole('heading', { name: 'Ranked by XLM balance' }),
    ).toBeInTheDocument();
    expect(screen.getByText('123,456 XLM')).toBeInTheDocument();
  });

  it('keeps the USD copy when the API ranked in USD', async () => {
    mockDirectory('usd');
    const header = renderDirectory();

    // The Panel is titled for USD before the response too, so wait on
    // the row the response brings rather than on the title.
    await screen.findByText('$123,456');
    expect(header).toHaveTextContent(/ranked by the total USD value/);
    expect(
      screen.getByRole('heading', { name: 'Ranked by USD wealth' }),
    ).toBeInTheDocument();
  });
});
