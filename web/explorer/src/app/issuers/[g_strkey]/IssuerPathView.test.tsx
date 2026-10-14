import { describe, it, expect, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});
vi.mock('@/lib/useLastPathSegment', () => ({ useLastPathSegment: () => G }));

import { apiGet } from '@/api/client';
import { IssuerPathView } from './IssuerPathView';

// The API nils `assets` on an errored or deadline-exceeded per-asset
// read (internal/api/v1/issuers.go); the shell must not read that as
// "issued nothing".
describe('IssuerPathView issued-asset list', () => {
  function renderView() {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    return render(
      <QueryClientProvider client={client}>
        <IssuerPathView />
      </QueryClientProvider>,
    );
  }
  function mockIssuer(detail: Record<string, unknown>) {
    vi.mocked(apiGet).mockImplementation(async (path: string) =>
      path.startsWith('/v1/issuers/') ? { data: detail } : { data: [] },
    );
  }

  it('says the list is unavailable when `assets` is absent', async () => {
    mockIssuer({ g_strkey: G });
    renderView();
    await waitFor(() =>
      expect(
        screen.getByText(/Issued-asset list unavailable/),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/No issued assets observed/),
    ).not.toBeInTheDocument();
  });

  it('keeps the genuine empty claim when `assets` is present and empty', async () => {
    mockIssuer({ g_strkey: G, assets: [] });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/No issued assets observed/)).toBeInTheDocument(),
    );
    expect(
      screen.queryByText(/Issued-asset list unavailable/),
    ).not.toBeInTheDocument();
  });

  it('separates an issuer the index never saw from a failed read', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/issuers/'))
        throw new Error(`404 Not Found on ${path}`);
      return { data: [] };
    });
    renderView();
    await waitFor(() =>
      expect(screen.getByText('Issuer not observed')).toBeInTheDocument(),
    );
    expect(screen.queryByText('Issuer unavailable')).not.toBeInTheDocument();
    expect(
      screen.queryByRole('button', { name: 'Retry' }),
    ).not.toBeInTheDocument();
  });

  it('offers a retry when the issuer read fails for any other reason', async () => {
    let calls = 0;
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/issuers/')) {
        calls += 1;
        if (calls === 1) throw new Error(`503 Service Unavailable on ${path}`);
        return { data: { g_strkey: G, assets: [] } };
      }
      return { data: [] };
    });
    renderView();
    await waitFor(() =>
      expect(screen.getByText('Issuer unavailable')).toBeInTheDocument(),
    );
    expect(screen.queryByText('Issuer not observed')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Retry' }));
    await waitFor(() =>
      expect(screen.getByText(/No issued assets observed/)).toBeInTheDocument(),
    );
  });

  it('says prices are unavailable instead of showing blank market columns', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.startsWith('/v1/issuers/'))
        return {
          data: {
            g_strkey: G,
            assets: [
              { asset_id: `USDC:${G}`, code: 'USDC', observation_count: 3 },
            ],
          },
        };
      throw new Error(`503 Service Unavailable on ${path}`);
    });
    renderView();
    await waitFor(() =>
      expect(screen.getByText(/Prices unavailable/)).toBeInTheDocument(),
    );
  });
});
