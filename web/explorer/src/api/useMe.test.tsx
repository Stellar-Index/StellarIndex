import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import type { ReactNode } from 'react';
import { renderHook, waitFor } from '@testing-library/react';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';

import { useMe } from './hooks';
import { SESSION_HINT_COOKIE, sessionHintPresent } from './sessionHint';

// The console line the operator reported — "Failed to load resource: the
// server responded with a status of 401" on every explorer page load —
// is written by the browser's own network layer, so no amount of error
// handling in `queryFn` can suppress it. The only fix is to not make the
// request when there is certainly no session to find. These tests pin
// the four states that produces.

function wrapper() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return function Wrapper({ children }: { children: ReactNode }) {
    return (
      <QueryClientProvider client={client}>{children}</QueryClientProvider>
    );
  };
}

function setHint() {
  document.cookie = `${SESSION_HINT_COOKIE}=1; Path=/`;
}

function dropHint() {
  document.cookie = `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/`;
}

function jsonResponse(body: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: String(status),
    json: async () => body,
  } as unknown as Response;
}

let fetchSpy: ReturnType<typeof vi.fn>;

beforeEach(() => {
  dropHint();
  fetchSpy = vi.fn();
  vi.stubGlobal('fetch', fetchSpy);
});

afterEach(() => {
  vi.unstubAllGlobals();
  dropHint();
});

describe('useMe session gating', () => {
  it('makes NO request at all when there is no session hint', async () => {
    const { result } = renderHook(() => useMe(), { wrapper: wrapper() });

    // Settled immediately: a disabled query is pending-but-idle, which
    // is `isLoading === false`. Consumers read undefined data and show
    // the signed-out surfaces, exactly as they do for a 401 today.
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(result.current.data).toBeUndefined();
    expect(result.current.isError).toBe(false);
  });

  it('probes and resolves the account when the hint is present and the session is valid', async () => {
    setHint();
    fetchSpy.mockResolvedValue(
      jsonResponse({ data: { user: { email: 'a@b.com' } } }),
    );

    const { result } = renderHook(() => useMe(), { wrapper: wrapper() });

    await waitFor(() => expect(result.current.data).toBeTruthy());
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    expect(result.current.data?.user?.email).toBe('a@b.com');
    // A valid session must not disturb the hint.
    expect(sessionHintPresent()).toBe(true);
  });

  it('degrades to signed-out AND drops the hint when it outlived the session', async () => {
    // The case that matters: the hint can survive server-side expiry,
    // revocation, or a cookie cleared on one side only. That must land
    // on today's behaviour (signed out), never on a broken signed-in
    // state — and must not repeat the 401 on the next page load.
    setHint();
    fetchSpy.mockResolvedValue(jsonResponse({}, 401));

    const { result } = renderHook(() => useMe(), { wrapper: wrapper() });

    await waitFor(() => expect(sessionHintPresent()).toBe(false));
    expect(fetchSpy).toHaveBeenCalledTimes(1);

    await waitFor(() => expect(result.current.isLoading).toBe(false));
    expect(result.current.data).toBeFalsy();
    expect(result.current.isError).toBe(false);

    // Having dropped the hint, the probe is disabled — no retry, no
    // five-minute refetch, nothing further on the wire.
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(fetchSpy).toHaveBeenCalledTimes(1);
  });

  it('never throws the 401 as a query error', async () => {
    setHint();
    fetchSpy.mockResolvedValue(jsonResponse({}, 401));

    const { result } = renderHook(() => useMe(), { wrapper: wrapper() });

    await waitFor(() => expect(fetchSpy).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(result.current.isLoading).toBe(false));
    // An errored probe is a DIFFERENT signal from "signed out" —
    // AccountGate renders a retry escape hatch for it rather than
    // bouncing to /signin, so a 401 must not land there.
    expect(result.current.isError).toBe(false);
  });

  it('still surfaces a genuine failure as an error, not as signed-out', async () => {
    setHint();
    fetchSpy.mockResolvedValue(jsonResponse({}, 503));

    const { result } = renderHook(() => useMe(), { wrapper: wrapper() });

    await waitFor(() => expect(result.current.isError).toBe(true));
    // A 5xx says nothing about whether the visitor is signed in, so the
    // hint must survive it — clearing it here would sign out every
    // visitor for the duration of an API outage.
    expect(sessionHintPresent()).toBe(true);
  });
});
