import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { AssetPathView } from './AssetPathView';

// The runtime-fallback shell for /assets/* slugs outside the build-time
// pre-render (2026-08-05, migration-0134 slugs; live report:
// /assets/usdt-gasu4kif 404'd while the listing linked to it).
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
    expect(container.firstChild).not.toBeNull();
  });

  it('does not throw on a malformed percent-escape segment', () => {
    expect(() => renderAtPath('/assets/USDT%ZZ')).not.toThrow();
  });
});
