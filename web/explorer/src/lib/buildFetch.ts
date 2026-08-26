/**
 * buildFetch — the build-time data layer for statically exported
 * (strategy-2) pages: /assets/[slug], /markets/[pair],
 * /issuers/[g_strkey], /sources/[name], /lending/[pool] and their
 * generateStaticParams. Server components in this app run ONLY at
 * `next build` (output: 'export'), so everything here is build-time
 * code — do NOT import it from 'use client' modules.
 *
 * ── Incident history (why this file exists) ─────────────────────────
 * Each strategy-2 page used to carry its own copy of timeout / retry /
 * memo scaffolding, and each copy re-learned the same lessons:
 *
 * - Baked "Asset not found" HTML: a 2s fetch timeout (later 8s) plus
 *   `catch { return null }` meant any build that hit a slow API window
 *   silently rendered the not-found branch INTO THE STATIC EXPORT for
 *   real entities (XLM itself shipped as "Asset not found" once).
 * - r1 rate-limit bursts: ~500 parallel per-slug fetches during export
 *   tripped the anonymous-tier 429 limit; unlucky slugs baked empty.
 * - Build hangs: duplicate un-memoised fetches across the casing +
 *   canonical-asset_id variants of one asset doubled API load and hung
 *   the CF Pages worker.
 * - XLM/WXLM 330x price bug: cache-key ambiguity served the wrapped-XLM
 *   row for slug "XLM" ($0.00067 vs ~$0.22). (Entity RESOLUTION still
 *   lives in the pages — this layer only guarantees transport.)
 *
 * ── The FAIL-HARD contract ───────────────────────────────────────────
 * This layer decouples deploys from live-API health by refusing to
 * guess:
 *
 * 1. Transport failures (network error, timeout, 5xx, non-JSON body)
 *    are retried with bounded backoff; if they persist, buildFetchData
 *    THROWS and `next build` FAILS. A failed build that keeps the last
 *    good deploy live is strictly better than a "successful" build
 *    that bakes not-found/empty HTML for real entities. A 502/503/504
 *    ("temporarily unavailable" — typically the API mid-deploy) is
 *    treated like a 429: a patient, Retry-After-aware wait that does NOT
 *    spend a transport attempt, bounded (MAX_UNAVAILABLE_WAITS) so a
 *    sustained outage still fails-hard rather than hanging the export.
 * 2. `null` is returned ONLY when the API answered authoritatively
 *    that the resource doesn't exist (4xx other than 429). Callers may
 *    treat that as a legitimate empty state — but for an entity that
 *    generateStaticParams promised, they must call failBuild() instead
 *    of rendering fallback HTML.
 * 3. The per-build memo (keyed by URL) dedupes repeated fetches across
 *    pages — including rejections, so one dead endpoint fails the
 *    build once, fast, instead of retrying per page.
 * 4. CI-stub builds (no network by design) get `null` back without any
 *    fetch, and failBuild() is a no-op — the CI fallback branches keep
 *    rendering so networkless smoke builds still pass.
 *
 * ── The `softFail` opt-out (build-time LIVE-PRICE enrichment) ─────────
 * Fail-hard is right for a page's core entity data (baking "not found"
 * for a real asset is the incident it guards against). It is WRONG for
 * the ephemeral live price: /v1/price is refreshed client-side by
 * <LivePrice> on every asset/market page, so a cold/slow/unreachable
 * price endpoint at build time should degrade the page (render without
 * the price — the client fills it in on hydration) rather than abort
 * the whole static export. The edge has been seen hanging ~25s on a
 * cold-cache asset, and it lands on a DIFFERENT asset each run, so any
 * single cold /v1/price must be non-fatal. Price fetches therefore pass
 * `{ softFail: true }` with a short timeout + few attempts; a persistent
 * transport failure/timeout then resolves to `null` instead of throwing.
 * NOTHING else opts in — the fail-hard contract for entity data (the
 * listing, per-asset detail, etc.) is unchanged.
 */

import { API_BASE_URL } from '@/api/client';
import type { components } from '@/api/types';

// AGT-06: the envelope's `flags`/`as_of` siblings (see EnvelopeMeta in the
// generated spec) used to be discarded entirely — only `data` survived
// buildFetchData's return. buildFetchEnvelope preserves them for callers
// that need the server's own stale/triangulated signal (e.g. the price
// enrichment on /assets/[slug]) instead of a client-synthesized guess.
export type BuildFetchEnvelope<T> = {
  data: T | null;
  flags?: components['schemas']['Flags'];
  as_of?: string;
};

/** True when the build host has no real API (CI placeholder URL). */
export const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

export class BuildFetchError extends Error {
  constructor(message: string) {
    super(message);
    this.name = 'BuildFetchError';
  }
}

// 8s per attempt: the API answers in <300ms steady-state; 8s covers a
// cold connection pool without letting one dead endpoint stall the
// export for minutes. (2s — the original value — caused the baked
// not-found incident; don't lower it.)
const DEFAULT_TIMEOUT_MS = 8_000;
const MAX_ATTEMPTS = 5;

// A 429 is NOT a transport failure — it is the server telling us to slow
// down, and it will keep saying so until the window rolls. Charging it
// against MAX_ATTEMPTS meant a throttled page burned its whole budget in
// ~10s of linear backoff and then failed the export; one launch-rehearsal
// build died exactly that way (passed on retry, which is the tell — nothing
// was broken, we just asked too fast). Throttling therefore gets its OWN
// budget and does not consume transport attempts.
const MAX_THROTTLE_WAITS = 8;

// A 502/503/504 during a build is almost always a TRANSIENT window, not a
// baked-in server bug: the API mid-deploy (its reverse proxy returns 502/503
// with no upstream) or a brief gateway blip. The 2026-08-26 explorer-deploy
// died on a SINGLE /v1/assets/LUKOIL-… 503 that raced the v0.44.3 API deploy —
// the old 5×`500*attempt` path gives up in ~5s, far short of an API restart.
// So these "temporarily unavailable" codes get the SAME discipline as a 429:
// wait on the server's terms (Retry-After aware, capped exponential backoff)
// WITHOUT spending a transport attempt. 6 waits over the shared 1s→30s ramp is
// ≈60s of patience — enough to ride a restart, while a genuinely SUSTAINED
// outage still exhausts the budget and fails-hard (keeping the last good
// deploy live). 500/501 are NOT here: a hard server error is more likely a real
// bug that should fail fast than a restart to wait out.
const MAX_UNAVAILABLE_WAITS = 6;
const UNAVAILABLE_STATUSES = new Set([502, 503, 504]);
export { MAX_UNAVAILABLE_WAITS };

// Exponential, capped. 1s,2s,4s,8s,16s,30s,30s,30s ≈ 2min of patience —
// comfortably longer than the anonymous tier's fixed window, so a build
// rides out a full window rather than dying inside it.
const THROTTLE_BASE_MS = 1_000;
const THROTTLE_CAP_MS = 30_000;

// Trust a server-sent Retry-After only up to this. A misconfigured or
// hostile value must not stall a build for hours.
const RETRY_AFTER_CAP_MS = 60_000;

/**
 * throttleDelayMs — how long to wait after a 429.
 *
 * Prefers the server's own Retry-After (seconds, or an HTTP-date) because
 * that is the only value that actually knows when the window rolls; falls
 * back to capped exponential backoff with jitter. Jitter matters: the export
 * fans out many pages, and un-jittered backoff re-synchronises them into the
 * next window together.
 */
export function throttleDelayMs(retryAfter: string | null, waitIndex: number): number {
  if (retryAfter) {
    const secs = Number(retryAfter);
    if (Number.isFinite(secs) && secs >= 0) {
      return Math.min(secs * 1_000, RETRY_AFTER_CAP_MS);
    }
    const at = Date.parse(retryAfter);
    if (!Number.isNaN(at)) {
      const delta = at - Date.now();
      if (delta > 0) return Math.min(delta, RETRY_AFTER_CAP_MS);
      return 0; // already elapsed
    }
  }
  const backoff = Math.min(THROTTLE_BASE_MS * 2 ** waitIndex, THROTTLE_CAP_MS);
  return backoff + Math.floor(Math.random() * 500);
}

// Per-build memo. Module state persists for the lifetime of the
// `next build` worker, which is exactly the scope we want.
const memo = new Map<string, Promise<unknown>>();

// One nonce per build process — see the edge-cache bypass note below.
const BUILD_NONCE = Date.now().toString(36);

/**
 * buildFetchData GETs `${API_BASE_URL}${path}`, unwraps the standard
 * `{data: T}` envelope, and memoises the result by URL for the whole
 * build.
 *
 * Returns `null` only for CI-stub builds or an authoritative 4xx
 * ("this resource does not exist"). THROWS BuildFetchError on
 * persistent transport failure — see the fail-hard contract above.
 *
 * Options:
 * - `timeoutMs` — per-attempt fetch timeout (default 8s).
 * - `attempts`  — max attempts before giving up (default 5).
 * - `softFail`  — when true, a persistent transport failure/timeout
 *   resolves to `null` INSTEAD of throwing. Reserved for build-time
 *   live-price enrichment (see the softFail note in the module header).
 *   Do NOT use for a page's core entity data.
 */
export function buildFetchData<T>(
  path: string,
  opts?: { timeoutMs?: number; attempts?: number; softFail?: boolean },
): Promise<T | null> {
  return buildFetchEnvelope<T>(path, opts).then((r) => r.data);
}

/**
 * buildFetchEnvelope is buildFetchData's full-envelope sibling: same
 * transport/retry/fail-hard contract, but resolves `{data, flags, as_of}`
 * instead of discarding everything but `data`. Use this when a caller
 * needs the server's own `flags` (e.g. `flags.stale`) rather than a
 * client-synthesized guess.
 */
export function buildFetchEnvelope<T>(
  path: string,
  opts?: { timeoutMs?: number; attempts?: number; softFail?: boolean },
): Promise<BuildFetchEnvelope<T>> {
  if (isCIStub) return Promise.resolve({ data: null });
  const base = `${API_BASE_URL}${path.startsWith('/') ? path : `/${path}`}`;
  // EDGE-CACHE BYPASS (2026-07-10): static-export builds must read the
  // ORIGIN's current truth, not whatever mix of Cloudflare cache entries
  // happens to be resident — two builds in one evening failed on stale
  // edge rows (a pre-v0.11 /v1/assets/{id} without `kind`; a pre-enable
  // /v1/sources without blend_emitter) that disagreed with sibling
  // fresh-cache responses INSIDE THE SAME BUILD. A per-build nonce query
  // param makes every build URL cache-unique, so requests go straight to
  // origin; the in-process memo still dedupes within the build, so
  // origin load per build is unchanged (one request per distinct path).
  const url = `${base}${base.includes('?') ? '&' : '?'}b=${BUILD_NONCE}`;
  const hit = memo.get(url);
  if (hit) return hit as Promise<BuildFetchEnvelope<T>>;
  const p = fetchWithRetry<T>(url, {
    timeoutMs: opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS,
    maxAttempts: opts?.attempts ?? MAX_ATTEMPTS,
    softFail: opts?.softFail ?? false,
  });
  memo.set(url, p);
  return p;
}

async function fetchWithRetry<T>(
  url: string,
  opts: { timeoutMs: number; maxAttempts: number; softFail: boolean },
): Promise<BuildFetchEnvelope<T>> {
  const { timeoutMs, maxAttempts, softFail } = opts;
  let lastErr: unknown = null;
  let throttleWaits = 0;
  let unavailableWaits = 0;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      const res = await fetch(url, {
        headers: { Accept: 'application/json' },
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (res.status === 429) {
        // Rate-limited: wait on the server's terms and DON'T spend a
        // transport attempt — see MAX_THROTTLE_WAITS. Waiting is the
        // correct response to a 429; retrying faster is not.
        lastErr = new Error('HTTP 429');
        if (throttleWaits < MAX_THROTTLE_WAITS) {
          await sleep(throttleDelayMs(res.headers.get('retry-after'), throttleWaits));
          throttleWaits++;
          attempt--; // this round was throttled, not attempted
        }
        continue;
      }
      if (res.status >= 400 && res.status < 500) {
        // Authoritative "does not exist" — the ONLY null-returning path
        // besides the CI stub. No retry.
        return { data: null };
      }
      if (UNAVAILABLE_STATUSES.has(res.status)) {
        // Temporarily unavailable (API mid-deploy / gateway blip). Wait on
        // the server's terms without spending a transport attempt — the same
        // discipline as a 429. A sustained outage exhausts the wait budget
        // and then fails-hard below, keeping the last good deploy live.
        lastErr = new Error(`HTTP ${res.status}`);
        if (unavailableWaits < MAX_UNAVAILABLE_WAITS) {
          await sleep(throttleDelayMs(res.headers.get('retry-after'), unavailableWaits));
          unavailableWaits++;
          attempt--; // this round was unavailable, not a spent attempt
        }
        continue;
      }
      if (!res.ok) {
        lastErr = new Error(`HTTP ${res.status}`);
        if (attempt < maxAttempts) await sleep(500 * attempt);
        continue;
      }
      // A 200 with a non-JSON or non-envelope body is an error payload,
      // not data — let it throw into the retry loop.
      const env = (await res.json()) as {
        data?: T;
        flags?: components['schemas']['Flags'];
        as_of?: string;
      };
      return { data: env.data ?? null, flags: env.flags, as_of: env.as_of };
    } catch (err) {
      lastErr = err;
      if (attempt < maxAttempts) await sleep(500 * attempt);
    }
  }
  if (softFail) {
    // Opt-in non-fatal caller (build-time live-price enrichment): the
    // value is refreshed client-side, so degrade the page rather than
    // abort the export. See the softFail note in the module header.
    return { data: null };
  }
  throw new BuildFetchError(
    `GET ${url} failed after ${maxAttempts} attempts — refusing to bake fallback HTML; fix the API (or the URL) and re-run the build. Last error: ${
      lastErr instanceof Error ? lastErr.message : String(lastErr)
    }`,
  );
}

/**
 * failBuild — call when a page that generateStaticParams promised
 * cannot be rendered truthfully (entity fetch returned null / empty).
 * Throws so `next build` fails instead of emitting not-found HTML for
 * a real entity. No-op on CI-stub builds so the networkless fallback
 * branches still render.
 */
export function failBuild(message: string): void {
  if (isCIStub) return;
  throw new BuildFetchError(`${message} (fail-hard: see src/lib/buildFetch.ts)`);
}

/**
 * requireRows — generateStaticParams helper: a listing that came back
 * null/empty on a real build means the API is unhealthy or the
 * contract broke; fail the build rather than silently exporting only
 * the fallback route. Under the CI stub returns [] so callers fall
 * through to their static fallback params.
 */
export function requireRows<T>(rows: T[] | null, what: string): T[] {
  if (!rows || rows.length === 0) {
    failBuild(`${what} returned no rows at build time`);
    return [];
  }
  return rows;
}

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
