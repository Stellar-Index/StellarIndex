import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, render } from '@testing-library/react';
import type { ReactNode } from 'react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

// Control the SSE layer directly: capture the listener subscribeStream is
// given, then push ledger frames at will. This drives the REAL path
// (subscribeStream → useStreamJSON → useLedgerStream → useLedgerFollow)
// rather than stubbing the hook under test.
const listeners: Array<(data: string) => void> = [];
vi.mock('./streams', () => ({
  subscribeStream: (_url: string, _type: string, onData: (d: string) => void) => {
    listeners.push(onData);
    return () => {};
  },
}));

const { resetLedgerFollowThrottleForTest, useLedgerFollow } = await import('./hooks');

// The defect (#470): `HomeTopMovers` and `HomeTopAssets` BOTH call
// `useLedgerFollow(['/v1/assets'])`. The throttle was a per-instance
// `useRef`, so it only ever throttled a component against itself. Both
// instances see the same SSE frame in one React commit, both start at 0,
// and neither can see the other — so every ledger advance produced two
// invalidations instead of one.
//
// Each invalidation matched BOTH /v1/assets queries on the page, and
// TanStack's invalidateQueries defaults to cancelRefetch: true, so the
// second one cancelled and restarted two in-flight fetches. Measured on
// r1: 224 bare /v1/assets requests from one browser in 30 minutes,
// arriving four-at-once.

function pushLedger(seq: number) {
  act(() => {
    for (const l of [...listeners]) {
      l(JSON.stringify({ data: { latest_ledger: seq } }));
    }
  });
}

let client: QueryClient;
let invalidateSpy: ReturnType<typeof vi.fn>;

function Wrapper({ children }: { children: ReactNode }) {
  return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
}

/** Renders N components that each follow `key`, as separate components
 * in one tree — the shape the home page actually has. */
function renderFollowers(keys: readonly (readonly unknown[])[]) {
  function Follower({ k }: { k: readonly unknown[] }) {
    useLedgerFollow(k);
    return null;
  }
  return render(
    <Wrapper>
      {keys.map((k, i) => (
        <Follower key={i} k={k} />
      ))}
    </Wrapper>,
  );
}

beforeEach(() => {
  listeners.length = 0;
  resetLedgerFollowThrottleForTest();
  client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  invalidateSpy = vi.fn();
  client.invalidateQueries = invalidateSpy as unknown as QueryClient['invalidateQueries'];
});

afterEach(() => {
  vi.useRealTimers();
});

describe('useLedgerFollow', () => {
  // THE regression. Two components, one key, one ledger advance → ONE
  // invalidation. Pre-fix this was 2.
  it('invalidates once when two components follow the same key', () => {
    renderFollowers([['/v1/assets'], ['/v1/assets']]);
    pushLedger(1000);

    expect(invalidateSpy).toHaveBeenCalledTimes(1);
  });

  // And it must keep holding as followers are added — the home page
  // gained its second follower without anyone noticing, and a third
  // would have made it six requests per advance.
  it('invalidates once no matter how many components follow the key', () => {
    renderFollowers([['/v1/assets'], ['/v1/assets'], ['/v1/assets'], ['/v1/assets']]);
    pushLedger(1000);

    expect(invalidateSpy).toHaveBeenCalledTimes(1);
  });

  // Blast radius: the throttle is per KEY, not global. A shared throttle
  // that suppressed unrelated panels would silently stop the rest of the
  // site from ticking — a worse bug than the one being fixed, and one
  // that would not show up as extra requests.
  it('does not throttle panels that follow different keys', () => {
    renderFollowers([['/v1/assets'], ['/v1/markets'], ['/v1/pools']]);
    pushLedger(1000);

    expect(invalidateSpy).toHaveBeenCalledTimes(3);
    const keys = invalidateSpy.mock.calls.map((c) => JSON.stringify(c[0].queryKey));
    expect(new Set(keys)).toEqual(
      new Set(['["/v1/assets"]', '["/v1/markets"]', '["/v1/pools"]']),
    );
  });

  // A redundant invalidation must not become redundant NETWORK traffic.
  // apiGet never forwards TanStack's AbortSignal, so a "cancelled"
  // refetch still completes on the wire — cancel-and-restart means we
  // pay twice and use one result.
  //
  // Asserted on the SECOND argument deliberately. `cancelRefetch` is an
  // InvalidateOptions field, not a filter: passing it inside the first
  // argument is a type error, and against a mocked client it would look
  // exactly like success while doing nothing in production. An earlier
  // draft of this test did precisely that and only `tsc` caught it.
  it('never cancels an in-flight refetch', () => {
    renderFollowers([['/v1/assets']]);
    pushLedger(1000);

    expect(invalidateSpy).toHaveBeenCalledWith(
      { queryKey: ['/v1/assets'] },
      { cancelRefetch: false },
    );
  });

  // The throttle must still WORK — it is a throttle, not a latch. A
  // second advance inside the window is suppressed; one after it is not.
  it('throttles repeat advances within the window and resumes after it', () => {
    // Realistic epoch values, not 0: an unset throttle entry reads as 0,
    // so a mocked "now" of 0 would look like "just refetched" and
    // suppress the first advance. Date.now() is ~1.7e12 in practice.
    const t0 = 1_756_000_000_000;
    const now = vi.spyOn(Date, 'now');
    now.mockReturnValue(t0);
    renderFollowers([['/v1/assets']]);

    pushLedger(1000);
    expect(invalidateSpy).toHaveBeenCalledTimes(1);

    now.mockReturnValue(t0 + 5_000); // inside the 15s window
    pushLedger(1001);
    expect(invalidateSpy).toHaveBeenCalledTimes(1);

    now.mockReturnValue(t0 + 20_000); // past it
    pushLedger(1002);
    expect(invalidateSpy).toHaveBeenCalledTimes(2);

    now.mockRestore();
  });
});
