import { act, renderHook } from '@testing-library/react';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { useLedgerStream } from './hooks';
import { resetStreamsForTest } from './streams';

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  url: string;
  readyState = FakeEventSource.OPEN;
  onopen: (() => void) | null = null;
  onerror: (() => void) | null = null;
  listeners = new Map<string, Set<(ev: MessageEvent) => void>>();

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
    this.readyState = FakeEventSource.CLOSED;
  }

  emit(type: string, data: string, lastEventId: string) {
    for (const fn of this.listeners.get(type) ?? []) {
      fn({ data, lastEventId } as MessageEvent);
    }
  }
}

// GH-1038: a hard-failed stream kept rendering its last frame as current
// until the caller's staleness window (30 s for the ledger tick) ran out,
// so a dead stream looked exactly like a quiet one.
describe('useLedgerStream connection status', () => {
  beforeEach(() => {
    FakeEventSource.instances = [];
    vi.stubGlobal('EventSource', FakeEventSource);
  });
  afterEach(() => {
    resetStreamsForTest();
    vi.unstubAllGlobals();
  });

  it('drops the last frame the moment the connection hard-fails', () => {
    const { result } = renderHook(() => useLedgerStream('https://api.test'));
    const es = FakeEventSource.instances[0];
    act(() => {
      es.onopen?.();
      es.emit(
        'ledger_update',
        '{"data":{"latest_ledger":100,"ingested_at":"2026-09-27T00:00:00Z","lag_seconds":1}}',
        '0198a4203f100001',
      );
    });
    expect(result.current?.data.latest_ledger).toBe(100);

    act(() => {
      es.readyState = FakeEventSource.CLOSED;
      es.onerror?.();
    });
    expect(result.current).toBeNull();
  });
});
