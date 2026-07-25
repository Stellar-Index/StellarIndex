import { describe, it, expect, vi, afterEach } from 'vitest';

import { apiGet, timeoutSignal } from './client';

// [absence: timeouts] every runtime fetch used to have no upper bound at
// all. timeoutSignal is the shared primitive that closes that gap while
// still honoring an external (e.g. TanStack Query) cancellation signal.
describe('timeoutSignal', () => {
  it('aborts on its own after the given timeout when no external signal is passed', async () => {
    vi.useFakeTimers();
    const signal = timeoutSignal(1_000);
    expect(signal.aborted).toBe(false);
    await vi.advanceTimersByTimeAsync(1_000);
    expect(signal.aborted).toBe(true);
    vi.useRealTimers();
  });

  it('aborts immediately when the external signal is already aborted', () => {
    const controller = new AbortController();
    controller.abort('external reason');
    const signal = timeoutSignal(60_000, controller.signal);
    expect(signal.aborted).toBe(true);
  });

  it('aborts when the external signal aborts before the timeout fires', async () => {
    vi.useFakeTimers();
    const controller = new AbortController();
    const signal = timeoutSignal(60_000, controller.signal);
    expect(signal.aborted).toBe(false);
    controller.abort('cancelled by caller');
    expect(signal.aborted).toBe(true);
    vi.useRealTimers();
  });

  it('does not abort before the timeout elapses', async () => {
    vi.useFakeTimers();
    const signal = timeoutSignal(5_000);
    await vi.advanceTimersByTimeAsync(4_000);
    expect(signal.aborted).toBe(false);
    vi.useRealTimers();
  });
});

describe('apiGet', () => {
  afterEach(() => vi.restoreAllMocks());

  it('passes a bounded AbortSignal to fetch (no more unbounded requests)', async () => {
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockResolvedValue(new Response(JSON.stringify({ ok: true }), { status: 200 }));
    await apiGet('/v1/status');
    expect(fetchSpy).toHaveBeenCalledTimes(1);
    const init = fetchSpy.mock.calls[0]?.[1] as RequestInit | undefined;
    expect(init?.signal).toBeInstanceOf(AbortSignal);
    expect(init?.signal?.aborted).toBe(false);
  });
});
