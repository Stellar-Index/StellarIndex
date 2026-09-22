import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { LendingPoolPathView } from './LendingPoolPathView';

// The runtime-fallback shell for /lending/[pool] outside the build-time
// pre-render (T278 — a pool the Blend factory spawns between builds
// hard-404'd on the static host with no functions/lending directory).
const POOL = 'CNEWPOOLDEPLOYEDAFTERLASTBUILDXXXXXXXXXXXXXXXXXXXXXXXX';

function renderAtPath(pathname: string) {
  window.history.pushState({}, '', pathname);
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <LendingPoolPathView />
    </QueryClientProvider>,
  );
}

describe('LendingPoolPathView', () => {
  it('renders the pool id read from the URL, not a build-time param', () => {
    const { getByText } = renderAtPath(`/lending/${POOL}`);
    expect(getByText(POOL)).toBeInTheDocument();
  });

  it('does not throw on a malformed percent-escape segment', () => {
    expect(() => renderAtPath('/lending/C%ZZ')).not.toThrow();
  });
});
