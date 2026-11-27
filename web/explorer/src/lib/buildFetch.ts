/**
 * buildFetch — the build-time data layer for statically exported
 * (strategy-2) pages: /assets/[slug], /markets/[pair],
 * /issuers/[g_strkey], /sources/[name], /lending/[pool] and their
 * generateStaticParams. Server components in this app run ONLY at
 * `next build` (output: 'export'), so everything here is build-time
 * code — do NOT import it from 'use client' modules.
 *
 * ── Incident history (why this file exists) ─────────────────────────
 * Strategy-2 pages share this timeout / retry / memo scaffolding
 * rather than each re-learning the same lessons:
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
 * 2. `null` is returned when the API answered authoritatively that the
 *    resource doesn't exist (404/410), or when a softFail caller hit a
 *    non-authoritative 4xx (400/401/403/405/…, e.g. an edge/WAF response —
 *    the API is unauthenticated, so it never issues those itself). Callers
 *    may treat that as a legitimate empty state — but for an entity that
 *    generateStaticParams promised, they must call failBuild() instead of
 *    rendering fallback HTML. A non-authoritative 4xx from a fail-hard
 *    (non-softFail) caller throws instead of collapsing to null — see (2b).
 * 2b. 408 is treated like a 502/503/504: a transient signal, retried with
 *    the same wait budget, never collapsed straight to null.
 * 3. The per-build memo (keyed by URL) dedupes repeated fetches across
 *    pages — including rejections, so one dead endpoint fails the
 *    build once, fast, instead of retrying per page.
 * 4. CI-stub builds (no network by design) get `null` back without any
 *    fetch, and failBuild() is a no-op — the CI fallback branches keep
 *    rendering so networkless smoke builds still pass.
 * 5. A degraded (partial) 200 from a core read is asked again a bounded
 *    number of times (DEGRADED_REASK_DELAYS_MS), within one summed sleep
 *    budget for the whole build (REASK_BUDGET_MS) and only while the route's
 *    re-asks still heal (REASK_UNHEALED_LIMIT); one still degraded is
 *    returned with `degraded: true` for the page to mark, and listed in the
 *    postbuild summary. It does not fail the build: the data is real, only
 *    incomplete, and one thin asset must not block a deploy.
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

import { createHash } from 'node:crypto';
import {
  mkdirSync,
  readdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from 'node:fs';
import { join } from 'node:path';

import { API_BASE_URL, buildHeaders } from '@/api/client';
import { CURRENT_NETWORK_ID } from './networks';
import type { components } from '@/api/types';
import {
  isPriceableAssetId,
  PRICE_BATCH_GET_MAX_IDS,
  type PriceBatchRow,
} from './price-batch';

// The envelope's `flags`/`as_of` siblings (see EnvelopeMeta in the
// generated spec) are dropped by buildFetchData — only `data` survives its
// return. buildFetchEnvelope preserves them for callers
// that need the server's own stale/triangulated signal (e.g. the price
// enrichment on /assets/[slug]) instead of a client-synthesized guess.
export type BuildFetchEnvelope<T> = {
  data: T | null;
  flags?: components['schemas']['Flags'];
  as_of?: string;
  /** /v1/price/batch only: the rows behind flags.stale / flags.triangulated. */
  stale?: string[];
  triangulated?: string[];
  pagination?: { next?: string | null };
  /** True when a REASK_DEGRADED_ROUTES answer was still degraded (partial) after the re-asks. */
  degraded?: boolean;
};

/**
 * True when the build must not reach the API: a CI placeholder URL, or
 * EXPLORER_BUILD_OFFLINE=1 (the status-page build, which must publish
 * during an API outage while still baking the real API URL for the client).
 */
export const isCIStub =
  API_BASE_URL.includes('.invalid') ||
  API_BASE_URL.includes('local-stub') ||
  process.env.EXPLORER_BUILD_OFFLINE === '1';

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
// with no upstream) or a brief gateway blip. A single 503 that races an
// API deploy must not fail the explorer build, and a short fixed backoff
// gives up far short of an API restart.
// So these "temporarily unavailable" codes get the SAME discipline as a 429:
// wait on the server's terms (Retry-After aware, capped exponential backoff)
// WITHOUT spending a transport attempt. 6 waits over the shared 1s→30s ramp is
// ≈60s of patience — enough to ride a restart, while a genuinely SUSTAINED
// outage still exhausts the budget and fails-hard (keeping the last good
// deploy live). 500/501 are NOT here: a hard server error is more likely a real
// bug that should fail fast than a restart to wait out.
// 408 (Request Timeout) joins this set: it is a proxy/edge signal that the
// request didn't complete in time, not an authoritative "not found" — it
// gets the same wait-then-retry discipline as a gateway blip rather than
// being collapsed into the 404/410 null bucket.
const MAX_UNAVAILABLE_WAITS = 6;
const UNAVAILABLE_STATUSES = new Set([408, 502, 503, 504]);
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
export function throttleDelayMs(
  retryAfter: string | null,
  waitIndex: number,
): number {
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

// Max requests in flight per build worker (next.config.mjs pins the export to one). Uncapped, one export sent ~17 rps of
// mostly cold long-tail reads, pushed API p95 to ~3s and paged slo_latency_burn.
export const MAX_IN_FLIGHT = 4;
let inFlight = 0;
const slotWaiters: Array<() => void> = [];

async function acquireSlot(): Promise<void> {
  if (inFlight < MAX_IN_FLIGHT) {
    inFlight++;
    return;
  }
  await new Promise<void>((resolve) => slotWaiters.push(resolve));
}

function releaseSlot(): void {
  const next = slotWaiters.shift();
  if (next) next();
  else inFlight--;
}

// The API prices a batch's rows 16 at a time against a DB pool shared with live
// /v1/price traffic, so batches run one at a time (inside the global cap above).
const PRICE_BATCH_ROUTE = '/v1/price/batch';
let batchTail: Promise<void> = Promise.resolve();

async function acquireBatchGate(): Promise<() => void> {
  const prev = batchTail;
  let release!: () => void;
  batchTail = new Promise<void>((resolve) => (release = resolve));
  await prev;
  return release;
}

// Per-build memo. Module state persists for the lifetime of the
// `next build` worker, which is exactly the scope we want.
const memo = new Map<string, Promise<unknown>>();

// The literal segments of the OpenAPI paths (lockstep-tested against the
// spec); any other segment of a request path is an id.
const ROUTE_WORDS = new Set(
  `account accounts activity admin aggregators anomalies archive assets at
  auctions auth backstop backups bad-debt batch begin-login begin-register begin-signup
  callback challenge changes chart claimable-balances code-history cohort
  config-history contracts coverage creators credentials cursors dashboard
  deliveries diagnostics directory divergence earnings emissions entry-changes
  export external finish-login finish-register finish-signup flows graph health
  healthz history holders incidents incidents.atom ingestion interactions
  issuers keys lake lastprice latest ledger ledgers lending liquidations
  liquidity-pools livez login logout lookup markets me metadata methodology mev
  movements network notices observations ohlc operations oracle orderbook pairs
  passkey pools positions premium price price-alerts prices protocols rates
  readyz register reserves resolve revenue rotate-secret rwa sac-wrappers sdex search
  sep10 series signer-on signup since-inception sources sponsors sponsorships
  stablecoins stats status status-notices stream streams supply throughput tip
  token top-accounts trades transactions transfers trustline-history tvl twap
  tx usage verified verify verify-code version vwap wasm webhooks
  x_last_price`.split(/\s+/),
);

/** routeTemplate maps a request path to its route: query dropped, each id segment `{id}`. */
export function routeTemplate(path: string): string {
  return path
    .split('?')[0]
    .split('/')
    .map((seg, i) => (i < 2 || ROUTE_WORDS.has(seg) ? seg : '{id}'))
    .join('/');
}

// The API marks a partial 200 (a cold section that missed its deadline) only by
// `Cache-Control: no-store` (Flags.Degraded never reaches the body) and keeps
// filling it in the background, so these core reads are asked again before
// the answer is baked. A pools answer heals within ~1.5 s; the others get one
// re-ask after one fill deadline. The waits hold no in-flight slot.
export const DEGRADED_REASK_DELAYS_MS: ReadonlyMap<string, readonly number[]> =
  new Map([
    ['/v1/assets', [4_000]],
    ['/v1/assets/{id}', [4_000]],
    ['/v1/markets', [4_000]],
    ['/v1/pools', [1_500, 4_000]],
  ]);

// Summed re-ask sleep the whole build (every process) may spend; past it a
// partial answer is baked marked. Bounds the build at minutes even when every
// answer is partial.
export const REASK_BUDGET_MS = 5 * 60_000;

// This many re-asks in a row on one route still partial means the API is
// replaying one cached copy: re-asking it again cannot heal, so stop.
export const REASK_UNHEALED_LIMIT = 8;

let reaskSleptMs = 0;
const unhealedCounts = new Map<string, number>();
const unhealedStreak = new Map<string, number>();
const reaskDisabled = new Set<string>();

function isDegradedAnswer(res: Response): boolean {
  return res.headers.get('cache-control')?.trim().toLowerCase() === 'no-store';
}

/** buildReaskSleptMs is this process's summed re-ask sleep. */
export function buildReaskSleptMs(): number {
  return reaskSleptMs;
}

/** buildReaskDisabled is the routes this process stopped re-asking. */
export function buildReaskDisabled(): ReadonlySet<string> {
  return reaskDisabled;
}

type SiblingTally = { reaskSleptMs?: number; reaskDisabled?: string[] };

// The other build processes' tallies, so the budget and a disabled route hold
// across `next build`'s render passes.
function siblingTallies(): SiblingTally[] {
  if (process.env.NEXT_PHASE !== 'phase-production-build') return [];
  let files: string[];
  try {
    files = readdirSync(COUNTS_DIR);
  } catch {
    return [];
  }
  const own = `${process.pid}.json`;
  return files
    .filter((f) => f.endsWith('.json') && f !== own)
    .map((f) => {
      try {
        return JSON.parse(
          readFileSync(join(COUNTS_DIR, f), 'utf8'),
        ) as SiblingTally;
      } catch {
        return {}; // mid-write: costs at most one re-ask over budget
      }
    });
}

// reserveReask books `delayMs` of the build's re-ask budget for `route`, or
// refuses when the route is disabled or the budget would be overspent.
function reserveReask(route: string, delayMs: number): boolean {
  const others = siblingTallies();
  if (
    reaskDisabled.has(route) ||
    others.some((t) => t.reaskDisabled?.includes(route))
  ) {
    return false;
  }
  const spent = others.reduce(
    (s, t) => s + (t.reaskSleptMs ?? 0),
    reaskSleptMs,
  );
  if (spent + delayMs > REASK_BUDGET_MS) return false;
  reaskSleptMs += delayMs;
  writeTally();
  return true;
}

function noteReaskOutcome(route: string, healed: boolean): void {
  if (healed) {
    unhealedStreak.set(route, 0);
    return;
  }
  unhealedCounts.set(route, (unhealedCounts.get(route) ?? 0) + 1);
  const streak = (unhealedStreak.get(route) ?? 0) + 1;
  unhealedStreak.set(route, streak);
  if (streak >= REASK_UNHEALED_LIMIT) reaskDisabled.add(route);
  writeTally();
}

// HTTP requests this process sent, per route, retries included: what the API sees.
const requestCounts = new Map<string, number>();
// Re-asks after a degraded answer, per route; paths baked degraded or unavailable.
const reaskCounts = new Map<string, number>();
const bakedDegraded = new Set<string>();
let warmupRequests = 0;

/** buildRequestCounts is this process's route → requests-sent tally. */
export function buildRequestCounts(): ReadonlyMap<string, number> {
  return requestCounts;
}

/** buildReaskCounts is this process's route → re-asks-after-degraded tally. */
export function buildReaskCounts(): ReadonlyMap<string, number> {
  return reaskCounts;
}

/** buildBakedDegraded is the request paths this process baked degraded or unavailable. */
export function buildBakedDegraded(): ReadonlySet<string> {
  return bakedDegraded;
}

// `next build` spreads pages over worker processes that are killed, not
// exited, so each rewrites its tally to .next (wiped at build start) and the
// postbuild step prints the sum.
const COUNTS_DIR = join(process.cwd(), '.next', 'build-fetch-counts');

/** buildWarmupRequests is the requests this process sent from buildWarmUp. */
export function buildWarmupRequests(): number {
  return warmupRequests;
}

function countRequest(route: string, warmUp: boolean): void {
  requestCounts.set(route, (requestCounts.get(route) ?? 0) + 1);
  if (warmUp) warmupRequests++;
  writeTally();
}

function countReask(route: string): void {
  reaskCounts.set(route, (reaskCounts.get(route) ?? 0) + 1);
}

function noteBakedDegraded(path: string): void {
  bakedDegraded.add(path);
  writeTally();
}

function writeTally(): void {
  if (process.env.NEXT_PHASE !== 'phase-production-build') return;
  try {
    mkdirSync(COUNTS_DIR, { recursive: true });
    writeFileSync(
      join(COUNTS_DIR, `${process.pid}.json`),
      JSON.stringify({
        requests: Object.fromEntries(requestCounts),
        reasked: Object.fromEntries(reaskCounts),
        degraded: [...bakedDegraded],
        warmup: warmupRequests,
        reaskSleptMs,
        reaskBudgetMs: REASK_BUDGET_MS,
        unhealed: Object.fromEntries(unhealedCounts),
        reaskDisabled: [...reaskDisabled],
        reaskUnhealedLimit: REASK_UNHEALED_LIMIT,
      }),
    );
  } catch {
    // A report-only tally: an unwritable .next costs the summary line, never the request.
  }
}

// One nonce per build process — see the edge-cache bypass note below.
const BUILD_NONCE = Date.now().toString(36);

/**
 * buildFetchData GETs `${API_BASE_URL}${path}`, unwraps the standard
 * `{data: T}` envelope, and memoises the result by URL for the whole
 * build.
 *
 * Returns `null` for CI-stub builds, an authoritative 404/410 ("this
 * resource does not exist"), or a softFail caller hitting a
 * non-authoritative 4xx. THROWS BuildFetchError on persistent transport
 * failure, or on a non-authoritative 4xx from a non-softFail caller —
 * see the fail-hard contract above.
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
 * buildFetchOptional is buildFetchData for decoration a page renders
 * without (the /embed widgets): same memo, in-flight cap and retries, but
 * any failure resolves to null instead of failing the build. It requests
 * with the fail-hard defaults, so a URL it shares with a fail-hard page is
 * memoised once with that page's contract.
 */
export function buildFetchOptional<T>(path: string): Promise<T | null> {
  return buildFetchData<T>(path).catch(() => null);
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
  const url = buildUrl(path);
  const hit = memo.get(url);
  if (hit) return hit as Promise<BuildFetchEnvelope<T>>;
  const p = fetchWithRetry<T>(url, path, {
    timeoutMs: opts?.timeoutMs ?? DEFAULT_TIMEOUT_MS,
    maxAttempts: opts?.attempts ?? MAX_ATTEMPTS,
    softFail: opts?.softFail ?? false,
    warmUp: false,
  });
  memo.set(url, p);
  return p;
}

function buildUrl(path: string): string {
  const base = `${API_BASE_URL}${path.startsWith('/') ? path : `/${path}`}`;
  // EDGE-CACHE BYPASS: static-export builds must read the
  // ORIGIN's current truth, not whatever mix of Cloudflare cache entries
  // happens to be resident — two builds in one evening failed on stale
  // edge rows (a pre-v0.11 /v1/assets/{id} without `kind`; a pre-enable
  // /v1/sources without blend_emitter) that disagreed with sibling
  // fresh-cache responses INSIDE THE SAME BUILD. A per-build nonce query
  // param makes every build URL cache-unique, so requests go straight to
  // origin; the in-process memo still dedupes within each worker process
  // (price batch chunks also across processes, via buildFetchPriceBatch).
  return `${base}${base.includes('?') ? '&' : '?'}b=${BUILD_NONCE}`;
}

// Warm-up stops starting requests after this, so a slow API costs the build
// minutes, not 2 × WARMUP keys × the 8 s timeout.
export const WARMUP_DEADLINE_MS = 3 * 60_000;

/**
 * buildWarmUp asks each path once, before the pages that read it render, so
 * the API starts filling a cold answer early. A complete answer is memoised
 * for the pages; a partial or failed one is discarded and never fails the
 * build. No re-asks; MAX_IN_FLIGHT lanes, so other reads interleave.
 */
export async function buildWarmUp(paths: readonly string[]): Promise<void> {
  if (isCIStub) return;
  const queue = [...new Set(paths)];
  const deadline = Date.now() + WARMUP_DEADLINE_MS;
  const lane = async () => {
    for (let path = queue.shift(); path !== undefined; path = queue.shift()) {
      if (Date.now() >= deadline) return;
      const url = buildUrl(path);
      if (memo.has(url)) continue;
      try {
        const env = await fetchWithRetry(url, path, {
          timeoutMs: DEFAULT_TIMEOUT_MS,
          maxAttempts: 1,
          softFail: true,
          warmUp: true,
        });
        if (env.data !== null && !env.degraded && !memo.has(url)) {
          memo.set(url, Promise.resolve(env));
        }
      } catch {
        // A warm-up is advisory: the page's own read applies its contract.
      }
    }
  };
  await Promise.all(Array.from({ length: MAX_IN_FLIGHT }, lane));
}

async function fetchWithRetry<T>(
  url: string,
  path: string,
  opts: {
    timeoutMs: number;
    maxAttempts: number;
    softFail: boolean;
    warmUp: boolean;
  },
): Promise<BuildFetchEnvelope<T>> {
  const { timeoutMs, maxAttempts, softFail, warmUp } = opts;
  const route = routeTemplate(path);
  const reaskDelays = DEGRADED_REASK_DELAYS_MS.get(route);
  // A warm-up neither re-asks nor lists a path: it bakes nothing.
  const noteDegraded = reaskDelays !== undefined && !warmUp;
  let lastErr: unknown = null;
  let throttleWaits = 0;
  let unavailableWaits = 0;
  let degradedReasks = 0;
  let reaskPending = false;
  for (let attempt = 1; attempt <= maxAttempts; attempt++) {
    try {
      // Taken before the global slot so a queued batch never idles on one;
      // both are released before any backoff sleep.
      const releaseBatch =
        route === PRICE_BATCH_ROUTE ? await acquireBatchGate() : undefined;
      let res: Response;
      try {
        await acquireSlot();
        try {
          countRequest(route, warmUp);
          res = await fetch(url, {
            headers: buildHeaders(),
            signal: AbortSignal.timeout(timeoutMs),
          });
        } finally {
          releaseSlot();
        }
      } finally {
        releaseBatch?.();
      }
      if (res.status === 429) {
        // Rate-limited: wait on the server's terms and DON'T spend a
        // transport attempt — see MAX_THROTTLE_WAITS. Waiting is the
        // correct response to a 429; retrying faster is not.
        lastErr = new Error('HTTP 429');
        if (throttleWaits < MAX_THROTTLE_WAITS) {
          await sleep(
            throttleDelayMs(res.headers.get('retry-after'), throttleWaits),
          );
          throttleWaits++;
          attempt--; // this round was throttled, not attempted
        }
        continue;
      }
      if (res.status === 404 || res.status === 410) {
        // Authoritative "does not exist" — the ONLY null-returning path
        // besides the CI stub (and a softFail non-authoritative 4xx below).
        // No retry.
        return { data: null };
      }
      if (UNAVAILABLE_STATUSES.has(res.status)) {
        // Temporarily unavailable (API mid-deploy / gateway blip). Wait on
        // the server's terms without spending a transport attempt — the same
        // discipline as a 429. A sustained outage exhausts the wait budget
        // and then fails-hard below, keeping the last good deploy live.
        lastErr = new Error(`HTTP ${res.status}`);
        if (unavailableWaits < MAX_UNAVAILABLE_WAITS) {
          await sleep(
            throttleDelayMs(res.headers.get('retry-after'), unavailableWaits),
          );
          unavailableWaits++;
          attempt--; // this round was unavailable, not a spent attempt
        }
        continue;
      }
      if (res.status >= 400 && res.status < 500) {
        // A non-authoritative 4xx (400/401/403/405/…). A 401/403 is a
        // rejected STELLARINDEX_BUILD_API_KEY or an intermediary (edge/WAF
        // bot-challenge) — treating it as "does not exist" would bake a
        // not-found page over a transport failure. A
        // softFail caller degrades gracefully (see the module header);
        // everything else fails the build so the contract break is visible
        // instead of silently baked in.
        if (softFail) {
          if (noteDegraded) noteBakedDegraded(path);
          return { data: null };
        }
        let extra = '';
        try {
          const body = (await res.json()) as { title?: string };
          if (body?.title) extra = ` — ${body.title}`;
        } catch {
          /* non-JSON body — keep the bare status line */
        }
        throw new BuildFetchError(
          `GET ${url} got HTTP ${res.status}${extra} — not an authoritative "not found"; refusing to bake a not-found page for a possible edge/WAF response or a rejected build API key.`,
        );
      }
      if (!res.ok) {
        lastErr = new Error(`HTTP ${res.status}`);
        if (attempt < maxAttempts) await sleep(500 * attempt);
        continue;
      }
      // A 200 with a non-JSON or non-envelope body is an error payload,
      // not data — let it throw into the retry loop.
      const env = (await res.json()) as Omit<BuildFetchEnvelope<T>, 'data'> & {
        data?: T;
      };
      const degraded = reaskDelays !== undefined && isDegradedAnswer(res);
      if (reaskPending) {
        noteReaskOutcome(route, !degraded);
        reaskPending = false;
      }
      const delay = reaskDelays?.[degradedReasks];
      if (
        degraded &&
        !warmUp &&
        delay !== undefined &&
        reserveReask(route, delay)
      ) {
        await sleep(delay);
        degradedReasks++;
        countReask(route);
        reaskPending = true;
        attempt--; // a partial answer is not a spent transport attempt
        continue;
      }
      if (degraded && noteDegraded) noteBakedDegraded(path);
      return {
        data: env.data ?? null,
        flags: env.flags,
        as_of: env.as_of,
        stale: env.stale,
        triangulated: env.triangulated,
        pagination: env.pagination,
        ...(degraded ? { degraded: true } : {}),
      };
    } catch (err) {
      lastErr = err;
      if (attempt < maxAttempts) await sleep(500 * attempt);
    }
  }
  if (softFail) {
    // Opt-in non-fatal caller (build-time live-price enrichment): the
    // value is refreshed client-side, so degrade the page rather than
    // abort the export. See the softFail note in the module header.
    if (noteDegraded) noteBakedDegraded(path);
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
  // Test nets legitimately have sparse/empty data (little on-chain activity,
  // SDEX-only, no aggregator/pricing) — an empty listing is the TRUE state,
  // not an unhealthy API, so we must NOT fail-hard the way mainnet does.
  // Export the shell/fallback routes (the same null-safe paths the CI stub
  // uses) and let the client fetch live from the test-net API at runtime.
  if (CURRENT_NETWORK_ID !== 'mainnet') return;
  throw new BuildFetchError(
    `${message} (fail-hard: see src/lib/buildFetch.ts)`,
  );
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

/** One /v1/price/batch row plus its per-row flags; `undefined` flags = the envelope could not say. */
export interface BuildPriceHit {
  row: PriceBatchRow;
  stale: boolean | undefined;
  triangulated: boolean | undefined;
}

const priceBatchMemo = new Map<string, Promise<Map<string, BuildPriceHit | null>>>();

// output: 'export' renders every page twice, the second pass in fresh worker
// processes, so each batch chunk is shared on disk to price it once per build
// (.next is wiped at build start). Keyed by path: the nonce differs per process.
const BATCH_MEMO_DIR = join(process.cwd(), '.next', 'build-price-batch');

function batchMemoFile(path: string): string {
  return join(BATCH_MEMO_DIR, `${createHash('sha256').update(path).digest('hex')}.json`);
}

function readBatchMemo(path: string): BuildFetchEnvelope<PriceBatchRow[]> | null {
  if (process.env.NEXT_PHASE !== 'phase-production-build') return null;
  try {
    const saved = JSON.parse(readFileSync(batchMemoFile(path), 'utf8')) as {
      path?: unknown;
      env?: BuildFetchEnvelope<PriceBatchRow[]>;
    };
    return saved.path === path && Array.isArray(saved.env?.data) ? saved.env : null;
  } catch {
    return null; // absent, partial or corrupt: ask the API
  }
}

function writeBatchMemo(path: string, env: BuildFetchEnvelope<PriceBatchRow[]>): void {
  if (process.env.NEXT_PHASE !== 'phase-production-build') return;
  const file = batchMemoFile(path);
  const tmp = `${file}.${process.pid}.tmp`;
  try {
    mkdirSync(BATCH_MEMO_DIR, { recursive: true });
    writeFileSync(tmp, JSON.stringify({ path, env }));
    renameSync(tmp, file);
  } catch {
    // The next process re-asks the API: costs a request, never a value.
  }
}

/**
 * buildFetchPriceBatch prices many ids against one quote through
 * /v1/price/batch, so a build costs one request per 100 ids instead of one
 * /v1/price per page. Result: id → hit, or null when the API answered "no
 * price" (absent or withheld). An id missing from the map went unanswered
 * (CI stub, failed chunk) — the caller falls back to a single-pair fetch.
 * Chunks go one at a time: each already fans out server-side.
 */
export function buildFetchPriceBatch(
  ids: readonly string[],
  quote: string,
): Promise<Map<string, BuildPriceHit | null>> {
  const wanted = [...new Set(ids)]
    .filter((id) => isPriceableAssetId(id) && id !== quote)
    .sort();
  const key = `${quote}|${wanted.join(',')}`;
  const hit = priceBatchMemo.get(key);
  if (hit) return hit;
  const p = (async () => {
    const out = new Map<string, BuildPriceHit | null>();
    for (let i = 0; i < wanted.length; i += PRICE_BATCH_GET_MAX_IDS) {
      const chunk = wanted.slice(i, i + PRICE_BATCH_GET_MAX_IDS);
      const path = `/v1/price/batch?asset_ids=${encodeURIComponent(chunk.join(','))}&quote=${encodeURIComponent(quote)}`;
      let env = readBatchMemo(path);
      if (!env) {
        env = await buildFetchEnvelope<PriceBatchRow[]>(path, {
          softFail: true,
          timeoutMs: 15_000,
          attempts: 2,
        });
        if (Array.isArray(env.data)) writeBatchMemo(path, env);
      }
      if (!env.data) continue;
      // Older APIs send only the OR-ed flag: a set flag without the id list
      // cannot be pinned to a row, so those rows' flags stay unknown.
      const flag = (set: boolean | undefined, list: string[] | undefined) =>
        list ? (id: string) => list.includes(id) : set ? () => undefined : () => false;
      const isStale = flag(env.flags?.stale, env.stale);
      const isTri = flag(env.flags?.triangulated, env.triangulated);
      for (const id of chunk) out.set(id, null);
      for (const row of env.data) {
        out.set(row.asset_id, {
          row,
          stale: isStale(row.asset_id),
          triangulated: isTri(row.asset_id),
        });
      }
    }
    return out;
  })();
  priceBatchMemo.set(key, p);
  return p;
}

// Price and the server's 24h change both come off a /v1/price/batch row
// (change_24h_pct exists only there). Rows are batched per quote over the
// whole build listing, so 500 pair pages cost a handful of requests; a pair
// outside the listing, or in a failed chunk, falls back to a one-id batch.
export async function buildFetchPairPrice(
  base: string,
  quote: string,
): Promise<PriceBatchRow | null> {
  let markets: { base: string; quote: string }[] | null = null;
  try {
    markets = await buildFetchData<{ base: string; quote: string }[]>(
      '/v1/markets?limit=500&order_by=volume_24h_usd_desc',
    );
  } catch {
    // The listing is fail-hard in markets/[pair]'s generateStaticParams; here it only
    // narrows the price fetch, so fall through to the one-id batch.
  }
  const bases = (markets ?? []).filter((m) => m.quote === quote).map((m) => m.base);
  return buildFetchBatchedPrice(base, quote, bases);
}

/**
 * buildFetchBatchedPrice prices one id from the memoised batch over
 * `universe` — every id the build prices against `quote` — so N pages cost
 * ⌈N/100⌉ requests. An id outside the universe, or in an unanswered chunk,
 * costs one one-id batch, the request each page used to make on its own.
 */
export async function buildFetchBatchedPrice(
  id: string,
  quote: string,
  universe: readonly string[],
): Promise<PriceBatchRow | null> {
  if (universe.includes(id)) {
    const hit = (await buildFetchPriceBatch(universe, quote)).get(id);
    if (hit !== undefined) return hit?.row ?? null;
  }
  const rows = await buildFetchData<PriceBatchRow[]>(
    `/v1/price/batch?asset_ids=${encodeURIComponent(id)}&quote=${encodeURIComponent(quote)}`,
    { softFail: true, timeoutMs: 6_000, attempts: 2 },
  );
  return rows?.[0] ?? null;
}
