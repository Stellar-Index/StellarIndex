'use client';

// Live-data hooks over the shared SSE multiplexer (see streams.ts).
// These are the building blocks of the "feels alive" program: pages
// subscribe to ledger closes and tip-price ticks and re-render as
// frames arrive, sharing one connection per endpoint across the tab.

import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';

import { API_BASE_URL } from '@/api/client';

import { subscribeStream } from './streams';

export interface StreamFrame<T> {
  data: T;
  /** Wall-clock arrival time — callers use this to drop a "live" badge
   * when the stream has gone silently quiet (WB-04). */
  receivedAt: number;
}

/**
 * useStreamJSON — subscribe to `eventType` frames on an SSE endpoint
 * and return the latest JSON-parsed payload. Pass `url: null` to
 * disable (hook order stays stable). Malformed frames keep the last
 * good value.
 */
export function useStreamJSON<T>(url: string | null, eventType: string): StreamFrame<T> | null {
  const [frame, setFrame] = useState<StreamFrame<T> | null>(null);
  // Reset during render when the url changes (the sanctioned
  // derived-state pattern) — a frame from the OLD pair must never be
  // served against the new one, and setState-in-effect cascades are a
  // lint error.
  const [prevUrl, setPrevUrl] = useState(url);
  if (url !== prevUrl) {
    setPrevUrl(url);
    setFrame(null);
  }

  useEffect(() => {
    if (!url) return;
    return subscribeStream(url, eventType, (data) => {
      try {
        setFrame({ data: JSON.parse(data) as T, receivedAt: Date.now() });
      } catch {
        // Keep the last good frame.
      }
    });
  }, [url, eventType]);

  return frame;
}

/**
 * useLiveClock — a wall-clock value that updates every `intervalMs`,
 * for staleness math in render (calling Date.now() during render is
 * impure under the react compiler). Starts at 0, meaning "no reading
 * yet": treat `clock === 0` as fresh — a frame's own arrival already
 * triggered a render, so the clock only needs to catch streams that
 * went QUIET (WB-04).
 */
export function useLiveClock(intervalMs = 10_000): number {
  const [now, setNow] = useState(0);
  useEffect(() => {
    const t = setInterval(() => setNow(Date.now()), intervalMs);
    return () => clearInterval(t);
  }, [intervalMs]);
  return now;
}

/** Staleness verdict from a live clock reading: false while the clock
 * has no reading yet (see useLiveClock). */
export function isFrameStale(clock: number, receivedAt: number, staleMs: number): boolean {
  return clock > 0 && clock - receivedAt > staleMs;
}

/** Payload of a /v1/ledger/stream `ledger_update` frame (and of
 * /v1/ledger/tip's data field). */
export interface LiveLedger {
  latest_ledger: number;
  ingested_at: string;
  lag_seconds: number;
}

/** A ledger tick is "live" while fresher than this. Ledgers close
 * ~every 5s; 30s of silence means the stream is wedged or the tab was
 * backgrounded — either way, stop claiming "live". */
export const LEDGER_LIVE_STALE_MS = 30_000;

/**
 * useLedgerStream — the latest network ledger close, shared tab-wide
 * over one connection. `apiBaseUrl` defaults to the configured API;
 * the status page passes per-region bases.
 */
export function useLedgerStream(apiBaseUrl: string = API_BASE_URL): StreamFrame<LiveLedger> | null {
  const frame = useStreamJSON<{ data: LiveLedger }>(
    `${apiBaseUrl}/v1/ledger/stream`,
    'ledger_update',
  );
  return frame ? { data: frame.data.data, receivedAt: frame.receivedAt } : null;
}

/** Payload of a /v1/price/tip/stream `tip_update` frame — the same
 * flattened envelope as GET /v1/price/tip (ADR-0003: prices are
 * decimal STRINGS). */
export interface LiveTip {
  data: { price: string; price_type?: string; window_seconds?: number };
  as_of: string;
  sources?: string[];
}

/**
 * useTipStream — live tip price for a pair. Pass `asset: null` to
 * disable (e.g. while the pair is unknown). Quote defaults to USD.
 * Pairs the server withholds (404/withheld pre-flight) hard-close the
 * stream; the multiplexer's slow-reopen keeps the retry cost near
 * zero and the hook simply stays null.
 */
export function useTipStream(asset: string | null, quote = 'fiat:USD'): StreamFrame<LiveTip> | null {
  const url = asset
    ? `${API_BASE_URL}/v1/price/tip/stream?asset=${encodeURIComponent(asset)}&quote=${encodeURIComponent(quote)}`
    : null;
  return useStreamJSON<LiveTip>(url, 'tip_update');
}

/** How long a tick flash stays visible. */
export const FLASH_MS = 900;

/**
 * usePriceFlash — compare successive price strings and return the tick
 * direction ('up' | 'down') for FLASH_MS after each change, else null.
 * Drive flash animations from this: className={flash === 'up' ? … }.
 */
export function usePriceFlash(price: string | null | undefined): 'up' | 'down' | null {
  const prev = useRef<string | null>(null);
  const [flash, setFlash] = useState<'up' | 'down' | null>(null);

  useEffect(() => {
    if (price == null || price === '') return;
    const last = prev.current;
    prev.current = price;
    if (last == null || last === price) return;
    const dir = Number(price) > Number(last) ? 'up' : 'down';
    setFlash(dir);
    const t = setTimeout(() => setFlash(null), FLASH_MS);
    return () => clearTimeout(t);
  }, [price]);

  return flash;
}

/** Default throttle for useLedgerFollow — ledgers close ~every 5s, but
 * most panels serve CDN-cached or 1-min-aggregated data, so refetching
 * on every close is wasteful. One refresh per 15s keeps the table
 * visibly moving without hammering the API. */
export const LEDGER_FOLLOW_REFRESH_MS = 15_000;

/**
 * useLedgerFollow — turn a static `useQuery` panel into a live one: on
 * each new ledger close (one SSE connection shared tab-wide via the
 * multiplexer), invalidate `queryKey`, throttled to at most once per
 * `minIntervalMs`. This is the canonical, cheapest way to make any
 * table/panel of on-chain data tick without a bespoke stream — it
 * generalises the MarketsTable/LedgersTable follow pattern (RT-2).
 *
 * Pass the query-key PREFIX to invalidate (TanStack matches by prefix),
 * e.g. `['/v1/liquidity-pools']` — it will refresh every query whose
 * key starts with that, regardless of trailing params.
 */
export function useLedgerFollow(
  queryKey: readonly unknown[],
  minIntervalMs: number = LEDGER_FOLLOW_REFRESH_MS,
): void {
  const frame = useLedgerStream();
  const queryClient = useQueryClient();
  const lastRefetchRef = useRef(0);
  const streamLatest = frame?.data.latest_ledger;
  // Serialise the key to a stable primitive so a fresh array literal
  // each render doesn't retrigger the effect; reconstruct inside.
  const keyStr = JSON.stringify(queryKey);
  useEffect(() => {
    if (streamLatest == null) return;
    const now = Date.now();
    if (now - lastRefetchRef.current < minIntervalMs) return;
    lastRefetchRef.current = now;
    void queryClient.invalidateQueries({ queryKey: JSON.parse(keyStr) as unknown[] });
  }, [streamLatest, queryClient, minIntervalMs, keyStr]);
}

/**
 * useObservationsFollow — event-driven liveness for ONE pair's trade data.
 * Subscribes to that pair's observations stream (`observations_update`) and
 * invalidates `queryKey` whenever a new trade lands, coalesced to
 * `minIntervalMs`. This makes a trade list refresh the instant the pair
 * trades — more precise than a ledger-wide follow. Opens one SSE connection
 * per pair, so use it on single-pair pages (not multi-pair boards) to respect
 * the per-IP stream cap. Pass `asset: null` to disable.
 */
export function useObservationsFollow(
  asset: string | null,
  quote: string,
  queryKey: readonly unknown[],
  minIntervalMs = 3_000,
): void {
  const url = asset
    ? `${API_BASE_URL}/v1/observations/stream?asset=${encodeURIComponent(asset)}&quote=${encodeURIComponent(quote)}`
    : null;
  const frame = useStreamJSON<unknown>(url, 'observations_update');
  const queryClient = useQueryClient();
  const lastRef = useRef(0);
  const receivedAt = frame?.receivedAt;
  const keyStr = JSON.stringify(queryKey);
  useEffect(() => {
    if (receivedAt == null) return;
    const now = Date.now();
    if (now - lastRef.current < minIntervalMs) return;
    lastRef.current = now;
    void queryClient.invalidateQueries({ queryKey: JSON.parse(keyStr) as unknown[] });
  }, [receivedAt, queryClient, minIntervalMs, keyStr]);
}
