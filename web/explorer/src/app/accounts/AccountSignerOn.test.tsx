import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';
import { AccountSignerOnPanel } from './AccountSignerOn';

const ME = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const OTHER = 'GDIEKKIQWMIZ4LD3RP3ABPN7X5KEAEWYMR634BRHB7EULIMEVREWLF3G';

const resp = (over = {}) => ({
  data: {
    account: ME,
    signer_on: [{ account: OTHER, weight: 2, last_modified_ledger: 64802900 }],
    through_ledger: 64802921,
    lower_bound: false,
    coverage_note: 'note',
    ...over,
  },
});

function renderPanel() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <AccountSignerOnPanel id={ME} />
    </QueryClientProvider>,
  );
}

afterEach(() => vi.mocked(apiGet).mockReset());

describe('AccountSignerOnPanel', () => {
  it('links each account the key signs for, with its weight', async () => {
    vi.mocked(apiGet).mockResolvedValue(resp());
    renderPanel();
    const link = await screen.findByTitle(OTHER);
    expect(link.getAttribute('href')).toBe(`/accounts/${OTHER}/`);
    expect(screen.getByText('Signer on (1)')).toBeInTheDocument();
    expect(screen.getByText('2')).toBeInTheDocument();
  });

  it('marks a capped read as a lower bound with its coverage note', async () => {
    vi.mocked(apiGet).mockResolvedValue(
      resp({
        lower_bound: true,
        coverage_note: 'older arrangements may be missing',
      }),
    );
    renderPanel();
    expect(await screen.findByText('Signer on (1+)')).toBeInTheDocument();
    expect(
      screen.getByText('older arrangements may be missing'),
    ).toBeInTheDocument();
  });

  it('renders nothing when the index is not built', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('503'));
    const { container } = renderPanel();
    await new Promise((r) => setTimeout(r, 0));
    expect(container.innerHTML).toBe('');
  });
});
