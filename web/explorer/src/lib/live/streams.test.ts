import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  activeStreamCount,
  resetStreamsForTest,
  STREAM_REOPEN_MS,
  subscribeStream,
} from './streams';

// Minimal EventSource fake: records instances, lets tests fire frames
// and hard failures. The multiplexer's contract under test: one
// connection per URL regardless of subscriber count, refcounted
// teardown with linger, slow reopen after a hard close.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  url: string;
  readyState = FakeEventSource.OPEN;
  onerror: (() => void) | null = null;
  listeners = new Map<string, Set<(ev: MessageEvent) => void>>();
  closed = false;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (ev: MessageEvent) => void) {
    let set = this.listeners.get(type);
    if (!set) {
      set = new Set();
      this.listeners.set(type, set);
    }
    set.add(fn);
  }

  close() {
    this.closed = true;
    this.readyState = FakeEventSource.CLOSED;
  }

  emit(type: string, data: string) {
    for (const fn of this.listeners.get(type) ?? []) {
      fn({ data } as MessageEvent);
    }
  }

  hardFail() {
    this.readyState = FakeEventSource.CLOSED;
    this.onerror?.();
  }
}

describe('subscribeStream', () => {
  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.instances = [];
    vi.stubGlobal('EventSource', FakeEventSource);
  });
  afterEach(() => {
    resetStreamsForTest();
    vi.unstubAllGlobals();
    vi.useRealTimers();
  });

  it('shares one connection across subscribers to the same URL', () => {
    const got: string[] = [];
    const un1 = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', (d) => got.push(`a:${d}`));
    const un2 = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', (d) => got.push(`b:${d}`));

    expect(FakeEventSource.instances).toHaveLength(1);
    FakeEventSource.instances[0].emit('ledger_update', '{"n":1}');
    expect(got).toEqual(['a:{"n":1}', 'b:{"n":1}']);

    un1();
    FakeEventSource.instances[0].emit('ledger_update', '{"n":2}');
    expect(got).toEqual(['a:{"n":1}', 'b:{"n":1}', 'b:{"n":2}']);
    un2();
  });

  it('distinct URLs get distinct connections', () => {
    const un1 = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    const un2 = subscribeStream('http://x/v1/price/tip/stream?asset=native', 'tip_update', () => {});
    expect(FakeEventSource.instances).toHaveLength(2);
    un1();
    un2();
  });

  it('closes the connection after the last unsubscribe + linger', () => {
    const un = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    un();
    expect(FakeEventSource.instances[0].closed).toBe(false);
    vi.advanceTimersByTime(6_000);
    expect(FakeEventSource.instances[0].closed).toBe(true);
    expect(activeStreamCount()).toBe(0);
  });

  it('re-subscribing within the linger window reuses the connection', () => {
    const un = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    un();
    const un2 = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    vi.advanceTimersByTime(10_000);
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].closed).toBe(false);
    un2();
  });

  it('schedules a slow reopen after a hard failure', () => {
    const got: string[] = [];
    const un = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', (d) => got.push(d));
    FakeEventSource.instances[0].hardFail();
    expect(FakeEventSource.instances).toHaveLength(1);

    vi.advanceTimersByTime(STREAM_REOPEN_MS + 1);
    expect(FakeEventSource.instances).toHaveLength(2);
    FakeEventSource.instances[1].emit('ledger_update', '{"n":3}');
    expect(got).toEqual(['{"n":3}']);
    un();
  });

  it('does not reopen after a hard failure once every subscriber left', () => {
    const un = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    FakeEventSource.instances[0].hardFail();
    un();
    vi.advanceTimersByTime(STREAM_REOPEN_MS * 2);
    expect(FakeEventSource.instances).toHaveLength(1);
  });

  it('unsubscribe is idempotent', () => {
    const un = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    const un2 = subscribeStream('http://x/v1/ledger/stream', 'ledger_update', () => {});
    un();
    un();
    // The double-call above must not have stolen un2's reference.
    vi.advanceTimersByTime(10_000);
    expect(FakeEventSource.instances[0].closed).toBe(false);
    un2();
  });
});
