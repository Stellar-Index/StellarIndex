import { describe, it, expect, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
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

  it('does not throw on a malformed percent-escape segment', () => {
    expect(() => renderAtPath('/assets/USDT%ZZ')).not.toThrow();
  });
});
