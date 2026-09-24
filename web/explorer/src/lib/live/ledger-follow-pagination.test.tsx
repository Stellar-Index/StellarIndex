import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import {
  act,
  fireEvent,
  render,
  screen,
  waitFor,
} from '@testing-library/react';
import type { ReactElement } from 'react';
import { beforeEach, describe, expect, it, vi } from 'vitest';

// Drive the real SSE → useLedgerStream → useLedgerFollow path by capturing
// the listener subscribeStream is handed and pushing ledger frames into it.
const listeners: Array<(data: string) => void> = [];
vi.mock('./streams', () => ({
  subscribeStream: (
    _url: string,
    _type: string,
    onData: (d: string) => void,
  ) => {
    listeners.push(onData);
    return () => {};
  },
}));

vi.mock('@/api/client', async () => {
  const actual =
    await vi.importActual<typeof import('@/api/client')>('@/api/client');
  return { ...actual, apiGet: vi.fn() };
});

const { apiGet } = await import('@/api/client');
const { resetLedgerFollowThrottleForTest } = await import('./hooks');
const { DexesView } = await import('@/app/dexes/DexesView');
const { VenueMarketsTable } = await import('@/components/VenueMarketsTable');

const PAGE2_CURSOR = 'page-2-cursor';

function pushLedger(seq: number) {
  act(() => {
    for (const l of [...listeners]) {
      l(JSON.stringify({ data: { latest_ledger: seq } }));
    }
  });
}

/** apiGet calls to `path` carrying `cursor` (undefined = the tip page). */
function fetchesOf(path: string, cursor: string | undefined): number {
  return vi
    .mocked(apiGet)
    .mock.calls.filter(
      ([p, params]) =>
        p === path &&
        (params as Record<string, unknown> | undefined)?.cursor === cursor,
    ).length;
}

async function settle() {
  await act(async () => {
    await new Promise((r) => setTimeout(r, 25));
  });
}

beforeEach(() => {
  listeners.length = 0;
  resetLedgerFollowThrottleForTest();
  vi.mocked(apiGet).mockReset();
  vi.mocked(apiGet).mockImplementation(async (_path, params) => {
    const atTip = !(params as Record<string, unknown> | undefined)?.cursor;
    return {
      data: [],
      ...(atTip ? { pagination: { next: PAGE2_CURSOR } } : {}),
    };
  });
});

// A keyset-paginated board must follow the tip only on its first page:
// a ledger-close nudge on page 2+ re-runs the cursor query and reshuffles
// the rows under the reader (volume order moves every ledger).
describe.each<[string, string, () => ReactElement]>([
  ['DexesView', '/v1/pools', () => <DexesView />],
  [
    'VenueMarketsTable',
    '/v1/markets',
    () => <VenueMarketsTable source="soroswap" title="Pools" rowNoun="pools" />,
  ],
])('%s ledger follow', (_name, path, ui) => {
  it('refreshes the tip page on a ledger close but not a deeper page', async () => {
    const client = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });
    render(<QueryClientProvider client={client}>{ui()}</QueryClientProvider>);
    await waitFor(() => expect(fetchesOf(path, undefined)).toBe(1));

    // Positive control: at the tip a ledger close does refetch, so the
    // assertion below is not passing merely because nothing ever fires.
    pushLedger(1000);
    await waitFor(() => expect(fetchesOf(path, undefined)).toBe(2));

    fireEvent.click(await screen.findByRole('button', { name: /Next/ }));
    await waitFor(() => expect(fetchesOf(path, PAGE2_CURSOR)).toBe(1));

    resetLedgerFollowThrottleForTest();
    pushLedger(1001);
    await settle();

    expect(fetchesOf(path, PAGE2_CURSOR)).toBe(1);
  });
});
