import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetTrustFacts, topTenShare } from './AssetTrustFacts';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

import { apiGet } from '@/api/client';

const G = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

function renderFacts(props: Partial<Parameters<typeof AssetTrustFacts>[0]>) {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetTrustFacts assetID={`USDC-${G}`} issuer={G} {...props} />
    </QueryClientProvider>,
  );
}

describe('topTenShare', () => {
  it('is exact above 2^53 and null for 10 or fewer rows', () => {
    const big = '90071992547409930000';
    const rows = [
      ...Array.from({ length: 10 }, () => ({ balance: big })),
      { balance: big },
    ];
    expect(topTenShare(rows)).toBe('90.9%');
    expect(topTenShare(rows.slice(0, 10))).toBeNull();
  });
});

describe('AssetTrustFacts', () => {
  it('renders each served fact with source and as-of, and lists the uncaptured', async () => {
    vi.mocked(apiGet).mockImplementation(async (path: string) => {
      if (path.includes('/holders')) {
        return {
          data: {
            holder_count: 1234,
            as_of_ledger: 63340102,
            holders: [{ account_id: 'GX', balance: '5' }],
          },
        } as never;
      }
      return {
        data: {
          g_strkey: G,
          creation_ledger: 1000,
          auth_required: false,
          auth_revocable: true,
          auth_flags_as_of_ledger: 777,
        },
      } as never;
    });
    renderFacts({ scamReason: 'impersonation' });
    await waitFor(() => expect(screen.getByText('1,234')).toBeTruthy());
    expect(screen.getByText(/created at ledger #1,000/)).toBeTruthy();
    expect(screen.getByText('on: auth_revocable')).toBeTruthy();
    expect(screen.getByText(/flagged: impersonation/)).toBeTruthy();
    expect(screen.getAllByText(/as of ledger 63,340,102/).length).toBe(1);
    expect(
      screen.getByText(/Not yet captured: .*volume character/),
    ).toBeTruthy();
  });

  it('never names the issuer-recorded domain as the directory source', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    for (const scamReason of ['impersonation', undefined]) {
      const { unmount } = renderFacts({
        scamReason,
        directoryDomain: 'evil.example',
      });
      await waitFor(() =>
        expect(screen.getByText(/issuer facts \(read failed\)/)).toBeTruthy(),
      );
      const src = screen.getByText(
        /source stellar\.expert community directory/,
      );
      expect(src.textContent).not.toContain('evil.example');
      expect(
        screen.getByText(/directory lists issuer domain: evil\.example/),
      ).toBeTruthy();
      unmount();
    }
  });

  it('says so when the issuer and holder reads fail', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderFacts({});
    await waitFor(() =>
      expect(screen.getByText(/issuer facts \(read failed\)/)).toBeTruthy(),
    );
    expect(screen.getByText(/holder facts \(read failed\)/)).toBeTruthy();
    expect(screen.getByText('no scam flag recorded')).toBeTruthy();
  });

  it('renders volume character with its window and drops it from the uncaptured list', async () => {
    vi.mocked(apiGet).mockRejectedValue(new Error('boom'));
    renderFacts({
      volumeCharacter: 'operational',
      volumeCharacterSignals: {
        window_days: 14,
        top_account_pair_vol_share: 1,
      },
    });
    await waitFor(() => expect(screen.getByText('operational')).toBeTruthy());
    expect(screen.getByText(/trailing 14-day window/)).toBeTruthy();
    const pending = screen.getByText(/Not yet captured:/).textContent ?? '';
    expect(pending).not.toContain('volume character');
    expect(pending).toContain('currency-authority fields');
  });

  it('shows a zero holder count and flags a missing one', async () => {
    const respond = (holder_count: number | null) =>
      vi.mocked(apiGet).mockImplementation(async (path: string) => {
        if (path.includes('/holders')) {
          return {
            data: { holder_count, as_of_ledger: 5, holders: [] },
          } as never;
        }
        return { data: { g_strkey: G } } as never;
      });
    respond(0);
    const first = renderFacts({});
    await waitFor(() => expect(screen.getByText('Holders')).toBeTruthy());
    expect(screen.getByText('0')).toBeTruthy();
    first.unmount();
    respond(null);
    renderFacts({});
    await waitFor(() =>
      expect(screen.getByText(/holder count \(not reported\)/)).toBeTruthy(),
    );
  });
});
