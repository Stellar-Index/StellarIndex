import { describe, it, expect } from 'vitest';
import { render } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { PairPathView } from './PairPathView';

// COR-09: useLastPathSegment() already swallows a failed decodeURIComponent
// and returns the raw (still-encoded-or-malformed) segment. PairPathView
// used to decode that value a SECOND time with no guard — for a segment
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
});
