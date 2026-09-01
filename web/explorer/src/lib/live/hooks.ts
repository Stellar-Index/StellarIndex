'use client';

// Live-data hooks over the shared SSE multiplexer (see streams.ts).
// These are the building blocks of the "feels alive" program: pages
// subscribe to ledger closes and tip-price ticks and re-render as
// frames arrive, sharing one connection per endpoint across the tab.

import { useQueryClient } from '@tanstack/react-query';
import { useEffect, useRef, useState } from 'react';

import { API_BASE_URL } from '@/api/client';
import { CURRENT_NETWORK } from '@/lib/networks';

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
function useStreamJSON<T>(
  url: string | null,
  eventType: string,
): StreamFrame<T> | null {
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
export function isFrameStale(
  clock: number,
  receivedAt: number,
  staleMs: number,
): boolean {
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
export function useLedgerStream(
  apiBaseUrl: string = API_BASE_URL,
): StreamFrame<LiveLedger> | null {
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
export function useTipStream(
  asset: string | null,
  quote = 'fiat:USD',
): StreamFrame<LiveTip> | null {
  // Test nets have no aggregator/pricing — /v1/price/tip/stream always 404s
  // there, so don't open it at all (an always-failing EventSource would just
  // retry forever). The pricing widgets render their empty state instead.
  const url =
    asset && CURRENT_NETWORK.pricing
      ? `${API_BASE_URL}/v1/price/tip/stream?asset=${encodeURIComponent(asset)}&quote=${encodeURIComponent(quote)}`
      : null;
  return useStreamJSON<LiveTip>(url, 'tip_update');
}

/** How long a tick flash stays visible. */
const FLASH_MS = 900;

/**
 * usePriceFlash — compare successive price strings and return the tick
 * direction ('up' | 'down') for FLASH_MS after each change, else null.
 * Drive flash animations from this: className={flash === 'up' ? … }.
 */
export function usePriceFlash(
  price: string | null | undefined,
): 'up' | 'down' | null {
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
const LEDGER_FOLLOW_REFRESH_MS = 15_000;

/**
 * Throttle state, keyed by the serialised query key and shared across
 * every hook instance — NOT per-component.
 *
 * It used to be a `useRef`, which meant the throttle only ever throttled
 * a component against itself. Two components following the same key
 * (`HomeTopMovers` and `HomeTopAssets` both follow `['/v1/assets']`)
 * observe the same SSE frame in one React commit, so both effects ran
 * back-to-back with both refs at 0, and neither could see the other.
 *
 * The cost was not one extra cache read. Each invalidation matched BOTH
 * `/v1/assets` queries on the page, and TanStack's `invalidateQueries`
 * defaults to `cancelRefetch: true` — so the second invalidation
 * cancelled and restarted two already-in-flight fetches. Four requests
 * per ledger advance where one component would have made two. Measured
 * on r1: 224 bare `/v1/assets` requests from a single browser in 30
 * minutes, arriving four-at-once (issue #470).
 *
 * Keyed by `keyStr` rather than global, so panels following different
 * keys never throttle each other. The map holds one small entry per
 * distinct key, and the keys come from call sites in source — a bounded
 * set — so it does not grow with traffic or time.
 */
const lastFollowRefetchByKey = new Map<string, number>();

/** Test hook: clear the shared throttle between cases. Mirrors
 * `resetStreamsForTest` in ./streams. */
export function resetLedgerFollowThrottleForTest(): void {
  lastFollowRefetchByKey.clear();
}

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
  enabled: boolean = true,
): void {
  const frame = useLedgerStream();
  const queryClient = useQueryClient();
  const streamLatest = frame?.data.latest_ledger;
  // Serialise the key to a stable primitive so a fresh array literal
  // each render doesn't retrigger the effect; reconstruct inside.
  const keyStr = JSON.stringify(queryKey);
  useEffect(() => {
    // `enabled` lets a paginated panel follow the tip only on its first
    // page — a keyset walk into history must NOT be yanked back to the
    // tip by a ledger-close nudge.
    if (!enabled) return;
    if (streamLatest == null) return;
    const now = Date.now();
    const last = lastFollowRefetchByKey.get(keyStr) ?? 0;
    if (now - last < minIntervalMs) return;
    lastFollowRefetchByKey.set(keyStr, now);
    void queryClient.invalidateQueries(
      { queryKey: JSON.parse(keyStr) as unknown[] },
      // Second argument — InvalidateOptions, NOT the filters object.
      // `cancelRefetch` is not a filter, and putting it in the first
      // argument type-errors rather than silently doing nothing.
      //
      // Do NOT cancel-and-restart a fetch already in flight. TanStack
      // defaults this to true, which turns a redundant invalidation into
      // redundant NETWORK traffic: the in-flight request is abandoned in
      // the cache and a fresh one issued. Worse here than usual, because
      // `apiGet` builds its own AbortSignal and never forwards the one
      // TanStack passes — so the abandoned request is not actually
      // cancelled on the wire and completes anyway. We pay for it twice
      // and use one result.
      //
      // This still matters with the shared throttle above: two panels
      // can follow OVERLAPPING prefixes rather than identical ones
      // (`['/v1/markets']` also matches `['/v1/markets', source]`), and
      // those are separate throttle keys by design.
      { cancelRefetch: false },
    );
  }, [streamLatest, queryClient, minIntervalMs, keyStr, enabled]);
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
    void queryClient.invalidateQueries({
      queryKey: JSON.parse(keyStr) as unknown[],
    });
  }, [receivedAt, queryClient, minIntervalMs, keyStr]);
}

/**
 * usePricePoll — the 60s /v1/price polling fallback that runs under the
 * tip stream (FEC audit A6-5: LiveAssetPrice, LivePairPrice, and the
 * embed LivePrice each hand-rolled this loop; the two page components now
 * share it — the embed stays standalone by design, bundle-light).
 *
 * Superset semantics (LiveAssetPrice's, the winning behavior): a 404
 * whose problem type ends in /price-withheld REPLACES the baked price
 * with the withheld verdict (the server deliberately refused to
 * aggregate; a lower-trust snapshot must not stand in). A plain 404 /
 * non-OK / network blip keeps whatever we have — never a blank flash.
 */
export function usePricePoll({
  asset,
  quote = 'fiat:USD',
  initialPrice = null,
  initialObservedAt = null,
  intervalMs = 60_000,
}: {
  asset: string;
  quote?: string;
  initialPrice?: number | null;
  initialObservedAt?: string | null;
  intervalMs?: number;
}): {
  price: number | null;
  observedAt: string | null;
  stale: boolean;
  triangulated: boolean;
  withheld: boolean;
  /** True after the first successful poll (including a withheld verdict). */
  polled: boolean;
} {
  const [state, setState] = useState({
    price: initialPrice,
    observedAt: initialObservedAt,
    stale: false,
    triangulated: false,
    withheld: false,
    polled: false,
  });

  useEffect(() => {
    // Test nets run no aggregator, so /v1/price always 404s (any quote) — the
    // poll would retry-storm and each failure logs a browser console error.
    // Skip it there; the hook keeps its initial/baked state (a native pair's
    // last price comes from /v1/pools, not this aggregator VWAP path).
    if (!CURRENT_NETWORK.pricing) return;
    let cancelled = false;
    const tick = async () => {
      try {
        const r = await fetch(
          `${API_BASE_URL}/v1/price?asset=${encodeURIComponent(asset)}&quote=${encodeURIComponent(quote)}`,
        );
        if (cancelled) return;
        if (r.status === 404) {
          const body = (await r.json().catch(() => null)) as {
            type?: string;
          } | null;
          if (!cancelled && body?.type?.endsWith('/price-withheld')) {
            setState((s) => ({
              ...s,
              price: null,
              withheld: true,
              polled: true,
            }));
          }
          return; // plain not-found: keep the baked value + caption
        }
        if (!r.ok) return;
        const body = (await r.json()) as {
          data?: { price?: string; observed_at?: string };
          flags?: { stale?: boolean; triangulated?: boolean };
        };
        const n = Number(body.data?.price);
        if (!Number.isFinite(n) || n <= 0) return;
        if (cancelled) return;
        setState({
          price: n,
          observedAt: body.data?.observed_at ?? null,
          stale: Boolean(body.flags?.stale),
          triangulated: Boolean(body.flags?.triangulated),
          withheld: false,
          polled: true,
        });
      } catch {
        // Network blip — keep whatever we have.
      }
    };
    void tick();
    const id = setInterval(() => void tick(), intervalMs);
    return () => {
      cancelled = true;
      clearInterval(id);
    };
  }, [asset, quote, intervalMs]);

  return state;
}
