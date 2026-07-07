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
 *    that bakes not-found/empty HTML for real entities.
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

// Per-build memo. Module state persists for the lifetime of the
// `next build` worker, which is exactly the scope we want.
const memo = new Map<string, Promise<unknown>>();

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
  if (isCIStub) return Promise.resolve(null);
  const url = `${API_BASE_URL}${path.startsWith('/') ? path : `/${path}`}`;
  const hit = memo.get(url);
  if (hit) return hit as Promise<T | null>;
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
): Promise<T | null> {
  const { timeoutMs, maxAttempts, softFail } = opts;
  let lastErr: unknown = null;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      const res = await fetch(url, {
        headers: { Accept: 'application/json' },
        signal: AbortSignal.timeout(timeoutMs),
      });
      if (res.status === 429) {
        // Rate-limited: longer, jittered backoff (the whole export runs
        // against r1's anonymous tier).
        lastErr = new Error('HTTP 429');
        if (attempt < maxAttempts) {
          await sleep(1_000 * attempt + Math.floor(Math.random() * 500));
        }
        continue;
      }
      if (res.status >= 400 && res.status < 500) {
        // Authoritative "does not exist" — the ONLY null-returning path
        // besides the CI stub. No retry.
        return null;
      }
      if (!res.ok) {
        lastErr = new Error(`HTTP ${res.status}`);
        if (attempt < maxAttempts) await sleep(500 * attempt);
        continue;
      }
      // A 200 with a non-JSON or non-envelope body is an error payload,
      // not data — let it throw into the retry loop.
      const env = (await res.json()) as { data?: T };
      return env.data ?? null;
    } catch (err) {
      lastErr = err;
      if (attempt < maxAttempts) await sleep(500 * attempt);
    }
  }
  if (softFail) {
    // Opt-in non-fatal caller (build-time live-price enrichment): the
    // value is refreshed client-side, so degrade the page rather than
    // abort the export. See the softFail note in the module header.
    return null;
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
