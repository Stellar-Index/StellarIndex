import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { PairPathView } from './PairPathView';

const apiGet = vi.hoisted(() => vi.fn());
vi.mock('@/api/client', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/api/client')>()),
  apiGet,
}));

const net = vi.hoisted(() => ({ id: 'mainnet' as 'mainnet' | 'testnet' }));
vi.mock('@/lib/networks', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/lib/networks')>();
  return {
    ...actual,
    get CURRENT_NETWORK() {
      return actual.NETWORKS.find((n) => n.id === net.id)!;
    },
  };
});

afterEach(() => {
  net.id = 'mainnet';
  apiGet.mockReset();
});

// useLastPathSegment() already swallows a failed decodeURIComponent
// and returns the raw (still-encoded-or-malformed) segment. PairPathView
// must not decode that value a SECOND time with no guard — for a segment
// containing an invalid percent-escape (e.g. a literal '%' not followed by
// two hex digits), the first decode attempt inside useLastPathSegment fails
// and is caught, but the second, unguarded decodeURIComponent call in
// PairPathView re-throws the same URIError, uncaught, crashing the page
// into the /markets segment error boundary instead of the "Unrecognised
// pair" EmptyState.
function renderAtPath(pathname: string) {
  window.history.pushState({}, '', pathname);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <PairPathView />
    </QueryClientProvider>,
  );
}

describe('PairPathView', () => {
  it('does not throw on a path segment with a malformed percent-escape', () => {
    expect(() => renderAtPath('/markets/TOKEN%ZZ~native')).not.toThrow();
  });

  it('skips /v1/price on a network without pricing', () => {
    net.id = 'testnet';
    renderAtPath('/markets/native~fiat:USD');
    expect(apiGet).not.toHaveBeenCalled();
    expect(screen.getByText('No prices on this network')).toBeTruthy();
  });
});
