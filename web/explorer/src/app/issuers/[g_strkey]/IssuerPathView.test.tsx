import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

vi.mock('@/api/client', async () => {
  const actual = await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('@/lib/useLastPathSegment', () => ({ useLastPathSegment: () => G }));

import { apiGet } from '@/api/client';
import { IssuerPathView } from './IssuerPathView';

// Frontend-honesty sweep: the long-tail issuer shell coalesced the
// soft-failed `assets` list to `[]` and claimed "No observed classic
// assets." — see internal/api/v1/issuers.go, which nils the list on an
// errored or deadline-exceeded per-asset read.
describe('IssuerPathView issued-asset list', () => {
  function renderView() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(
      <QueryClientProvider client={client}>
        <IssuerPathView />
      </QueryClientProvider>,
    );
  }

  it('says the list is unavailable when `assets` is absent', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { g_strkey: G } });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/Issued-asset list unavailable/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/No observed classic assets/)).not.toBeInTheDocument();
  });

  it('keeps the genuine empty claim when `assets` is present and empty', async () => {
    vi.mocked(apiGet).mockResolvedValue({ data: { g_strkey: G, assets: [] } });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/No observed classic assets/)).toBeInTheDocument(),
    );
    expect(screen.queryByText(/Issued-asset list unavailable/)).not.toBeInTheDocument();
  });
});
