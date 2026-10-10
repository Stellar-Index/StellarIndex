// The /accounts ranking basis is the SERVED `ranked_by`, stated once on
// the table's Panel title; the page header states no basis, so it can
// never claim USD above a table ranked in XLM (the API falls back to
// native XLM whenever its price catalogue degrades, mainnet included).
//
// The test env carries no NEXT_PUBLIC_NETWORK, so this is a MAINNET
// build (pricing: true).
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
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
function mockDirectory(ranked_by: 'usd' | 'native_xlm', value = '123456') {
  vi.mocked(apiGet).mockImplementation((path: string) => {
    if (path === '/v1/accounts') {
      return Promise.resolve({
        data: {
          priced_assets: ranked_by === 'usd' ? 12 : 0,
          ranked_by,
          accounts: [{ account_id: G, value }],
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
  const header = container.querySelector('h1')?.parentElement;
  if (!header) throw new Error('directory header did not render');
  return header;
}

describe('AccountsDirectory — ranking basis follows the served ranked_by', () => {
  it('titles the table for XLM when the API ranked in XLM', async () => {
    mockDirectory('native_xlm');
    const header = renderDirectory();

    expect(header.querySelector('h1')).toHaveTextContent('Accounts');
    await screen.findByText('123,456 XLM');
    expect(header).not.toHaveTextContent(/USD|ranked by/i);
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
    expect(header).not.toHaveTextContent(/ranked by/i);
    expect(
      screen.getByRole('heading', { name: 'Ranked by USD wealth' }),
    ).toBeInTheDocument();
  });
});

describe('AccountsDirectory wealth column', () => {
  it('rounds USD wealth exactly from the decimal string', async () => {
    mockDirectory('usd', '1234.4999999999999999');
    renderDirectory();
    expect(await screen.findByText('$1,234')).toBeInTheDocument();
  });

  it('keeps every digit of an XLM balance above 2^53', async () => {
    mockDirectory('native_xlm', '9007199254740993');
    renderDirectory();
    expect(
      await screen.findByText('9,007,199,254,740,993 XLM'),
    ).toBeInTheDocument();
  });
});
