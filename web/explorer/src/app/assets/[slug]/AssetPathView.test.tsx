import { describe, it, expect, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn(() => new Promise(() => {})) };
});

import { apiGet } from '@/api/client';
import { AssetPathView } from './AssetPathView';

// The runtime-fallback shell for /assets/* slugs outside the build-time
// pre-render. The detail fetch never settles, so the view stays pending.
function renderAtPath(pathname: string) {
  window.history.pushState({}, '', pathname);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <AssetPathView />
    </QueryClientProvider>,
  );
}

describe('AssetPathView', () => {
  it('renders without throwing and shows a loading state for a slug path', () => {
    const { container } = renderAtPath('/assets/usdt-gasu4kif');
    expect(apiGet).toHaveBeenCalledWith('/v1/assets/usdt-gasu4kif');
    // Only text-free skeleton placeholders: no not-found verdict and no
    // asset detail before the fetch settles.
    expect(screen.queryByText('Asset not found')).toBeNull();
    expect(screen.queryByText('Canonical asset id')).toBeNull();
    expect(container.textContent).toBe('');
    expect(container.firstElementChild?.children.length).toBeGreaterThan(0);
  });

  it.each([
    ['/assets/crypto:BTC', '/external/assets/btc/'],
    ['/assets/raw:BTC', '/oracles/'],
  ])('sends %s to %s instead of querying /v1/assets', (path, target) => {
    const replace = vi.fn();
    const loc = window.location;
    Object.defineProperty(window, 'location', {
      configurable: true,
      value: { pathname: path, replace },
    });
    vi.mocked(apiGet).mockClear();
    try {
      render(
        <QueryClientProvider client={new QueryClient()}>
          <AssetPathView />
        </QueryClientProvider>,
      );
      expect(replace).toHaveBeenCalledWith(target);
      expect(apiGet).not.toHaveBeenCalled();
    } finally {
      Object.defineProperty(window, 'location', {
        configurable: true,
        value: loc,
      });
    }
  });

  it('does not throw on a malformed percent-escape segment', () => {
    expect(() => renderAtPath('/assets/USDT%ZZ')).not.toThrow();
  });

  it('rounds 24h volume above 2^53 from the exact decimal', async () => {
    vi.mocked(apiGet).mockResolvedValueOnce({
      data: {
        asset_id: 'USDT-GASU4KIF',
        code: 'USDT',
        volume_24h_usd: '1000000004999999999',
      },
    });
    renderAtPath('/assets/usdt-gasu4kif');
    await waitFor(() =>
      expect(screen.getByText('$1,000,000T')).toBeInTheDocument(),
    );
  });
  it('titles a SEP-41 contract token as a Soroban token', async () => {
    vi.mocked(apiGet).mockResolvedValueOnce({
      data: { asset_id: 'CAS3J7GY', type: 'soroban', code: 'BLND' },
    });
    renderAtPath('/assets/blnd');
    await waitFor(() =>
      expect(document.title).toBe('BLND — Soroban token · Stellar Index'),
    );
  });
});
