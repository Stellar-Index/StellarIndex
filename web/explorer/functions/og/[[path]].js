import { ImageResponse } from 'workers-og';

// Dynamic OG card generator (SEO plan D7). GET /og/{type}/{id} → a 1200×630 PNG
// (satori + resvg-wasm on CF Pages Functions), edge-cached. Market cards carry
// the LIVE price (near-real-time via the 60s edge cache + a tight upstream
// timeout, guarded by a circuit breaker below). NB the live fetch hits our
// public API from the edge; a per-IP rate limit (K067) bounds a single
// client's request volume in addition to the OG_DISABLED kill-switch.

function code(s) {
  if (!s) return '';
  if (s === 'native') return 'XLM';
  const colon = s.indexOf(':');
  if (colon > 0) s = s.slice(colon + 1);
  const dash = s.indexOf('-');
  return (dash > 0 ? s.slice(0, dash) : s).toUpperCase();
}

function prettyLabel(type, id) {
  if (!id) return null;
  if (type === 'markets' && id.includes('~')) {
    const [b, q] = id.split('~');
    return `${code(b)} / ${code(q)}`;
  }
  if (type === 'assets') return code(id);
  return id.length > 24 ? `${id.slice(0, 10)}…${id.slice(-8)}` : id;
}

// SEC-08: a Map, not a plain object — a plain object keyed by an
// attacker-controlled `type` string lets `type === 'constructor'` (etc.)
// resolve an inherited Object.prototype member instead of missing cleanly.
export const TYPE_LABEL = new Map([
  ['markets', 'Market'],
  ['assets', 'Asset'],
  ['transactions', 'Transaction'],
  ['ledgers', 'Ledger'],
  ['accounts', 'Account'],
  ['contracts', 'Contract'],
  ['protocols', 'Protocol'],
  ['issuers', 'Issuer'],
]);

// input-validation: bound the id segment before it reaches decode, the
// upstream price fetch, or the satori render.
const MAX_ID_LENGTH = 160;

// SEC-15: liveSubline's upstream fetch is only worth making — and only
// safe to make unauthenticated, at the edge, on every request — when both
// legs of `base~quote` actually look like a canonical asset id the API
// accepts: `native`, `CODE-ISSUER` (up to a 12-char code + '-' + a 56-char
// G/C-strkey issuer), `pool:<hex>`, `fiat:<code>`, or a plain slug — never
// arbitrary attacker-controlled text (markup, whitespace, path traversal).
const ASSET_LEG_RE = /^[A-Za-z0-9_:-]{1,80}$/;

// K067: a per-IP token bucket. This is deliberately module-scope state, not
// KV — CF Pages Functions reuse an isolate across many requests, so this is
// a real (if best-effort, per-isolate) gate against a single client hammering
// the render path, layered on top of (not replacing) the OG_DISABLED
// dashboard kill-switch for a sustained/distributed flood.
const RATE_LIMIT_WINDOW_MS = 10_000;
const RATE_LIMIT_MAX_PER_WINDOW = 20;
let rateLimitBuckets = new Map(); // ip -> { count, windowStart }

function isRateLimited(ip) {
  const now = Date.now();
  const bucket = rateLimitBuckets.get(ip);
  if (!bucket || now - bucket.windowStart >= RATE_LIMIT_WINDOW_MS) {
    rateLimitBuckets.set(ip, { count: 1, windowStart: now });
    // Opportunistic eviction so an unbounded stream of distinct IPs can't
    // grow this map forever within one isolate's lifetime.
    if (rateLimitBuckets.size > 5000) rateLimitBuckets.clear();
    return false;
  }
  bucket.count += 1;
  return bucket.count > RATE_LIMIT_MAX_PER_WINDOW;
}

// F101: liveSubline's upstream fetch would otherwise run again on every
// single markets-type request even while the upstream is down — a
// per-isolate circuit breaker backs off instead of hammering it. Best
// effort (state resets on isolate recycle), not a distributed limiter.
const UPSTREAM_BREAKER_THRESHOLD = 5;
const UPSTREAM_BREAKER_COOLDOWN_MS = 30_000;
let upstreamFailures = 0;
let upstreamOpenUntil = 0;

function upstreamBreakerOpen() {
  return Date.now() < upstreamOpenUntil;
}

function recordUpstreamOutcome(ok) {
  if (ok) {
    upstreamFailures = 0;
    upstreamOpenUntil = 0;
    return;
  }
  upstreamFailures += 1;
  if (upstreamFailures >= UPSTREAM_BREAKER_THRESHOLD) {
    upstreamOpenUntil = Date.now() + UPSTREAM_BREAKER_COOLDOWN_MS;
  }
}

// T270: the old fixed-precision formatter here dropped into exponential
// notation below 1e-6 (e.g. "7.407e-7"), the same scientific-notation
// regression `@/lib/format`'s formatSubunitPrice
// was written to avoid (2026-08-06: "is not user-friendly"). This
// function isn't reachable from `src/lib/format.ts` — Pages Functions
// bundle standalone from the app — so it mirrors formatSubunitPrice's
// plain-decimal, trimmed-trailing-zero behavior instead of importing it.
function formatSubPriceDecimal(n, sig = 4) {
  const abs = Math.abs(n);
  if (abs === 0) return '0';
  const leadingZeros = Math.max(0, -Math.floor(Math.log10(abs)) - 1);
  const decimals = Math.min(leadingZeros + sig, 20);
  let out = n.toFixed(decimals);
  if (out.includes('.')) {
    out = out.replace(/0+$/, '').replace(/\.$/, '');
  }
  return out;
}

// Exported for unit tests only (functions/og/og.test.js) — resets the
// in-module breaker and rate-limit state so one test's failures/requests
// don't leak into another.
export function resetOgGatesForTest() {
  upstreamFailures = 0;
  upstreamOpenUntil = 0;
  rateLimitBuckets = new Map();
}

// Exported for unit tests only (functions/og/og.test.js) — CF Pages only
// invokes `onRequest`; these named exports have no runtime effect on it.
// `apiOrigin` is required rather than defaulted: each network's explorer
// runs as its own Pages project on its own hostname ({network}.
// stellarindex.io, bare for mainnet) with its own DNS-only API origin
// (api.{that same hostname}), and onRequest derives it from the inbound
// request — this function has no business guessing a network.
export async function liveSubline(type, rawId, apiOrigin) {
  try {
    if (type === 'markets' && rawId.includes('~')) {
      const [base, quote] = rawId.split('~');
      if (!ASSET_LEG_RE.test(base) || !ASSET_LEG_RE.test(quote)) return null;
      if (upstreamBreakerOpen()) return null;
      const r = await fetch(
        `${apiOrigin}/v1/price?asset=${encodeURIComponent(base)}&quote=${encodeURIComponent(quote)}`,
        {
          signal: AbortSignal.timeout(2500),
          headers: { 'user-agent': 'stellarindex-og/1' },
        },
      );
      recordUpstreamOutcome(r.ok);
      if (r.ok) {
        const p = (await r.json())?.data?.price;
        if (p != null) {
          const n = Number(p);
          const fmt =
            n >= 1
              ? n.toLocaleString('en-US', { maximumFractionDigits: 2 })
              : formatSubPriceDecimal(n);
          return `1 ${code(base)} = ${fmt} ${code(quote)}`;
        }
      }
    }
  } catch {
    recordUpstreamOutcome(false);
    /* fall through to label-only card */
  }
  return null;
}

// F101: a rejection is fully determined by the URL (unknown type / oversized
// id never becomes valid), so the edge should serve it from cache instead of
// re-running the gate on every repeat hit — same amplification risk the 200
// path's cache-control already guards against.
const NOT_FOUND_CACHE_CONTROL = 'public, max-age=60, s-maxage=3600';

function notFound() {
  return new Response('Not found', {
    status: 404,
    headers: { 'cache-control': NOT_FOUND_CACHE_CONTROL },
  });
}

export async function onRequest(context) {
  const { request, env } = context;

  // kill-switch: flip OG_DISABLED=1 as a CF Pages environment variable
  // (dashboard-only, no redeploy) to take this endpoint offline if it's
  // ever driving abusive load.
  if (env?.OG_DISABLED === '1') {
    return new Response('OG image generation is temporarily disabled.', {
      status: 503,
    });
  }

  // K067: per-IP gate ahead of everything else — cheaper than the 404
  // checks below and the only one of these gates that needs the request's
  // origin, not just its path.
  const clientIP = request.headers.get('cf-connecting-ip') || 'unknown';
  if (isRateLimited(clientIP)) {
    return new Response('Too many requests', {
      status: 429,
      headers: { 'retry-after': '10' },
    });
  }

  const url = new URL(request.url);
  const parts = url.pathname
    .replace(/^\/og\/?/, '')
    .split('/')
    .filter(Boolean);
  const type = (parts[0] || 'home').replace(/[^a-z0-9-]/gi, '');

  // SEC-15: 404 unknown types before doing any work — 'home' is the only
  // pseudo-type without a TYPE_LABEL entry (the id-less site-wide card).
  if (type !== 'home' && !TYPE_LABEL.has(type)) {
    return notFound();
  }

  let rawId = parts.slice(1).join('/') || '';
  // input-validation: reject oversized ids before decode/fetch/render.
  if (rawId.length > MAX_ID_LENGTH) {
    return notFound();
  }
  // CS-009: decode the path segment AT MOST ONCE (the previous 2× loop
  // defeated the upstream ogImageFor encodeURIComponent, resurfacing raw
  // markup). Combined with esc() below this closes the SSRF/injection sink.
  try {
    rawId = decodeURIComponent(rawId);
  } catch {
    /* leave as-is */
  }
  const label =
    prettyLabel(type, rawId) || 'Stellar pricing & protocol explorer';
  const kicker = TYPE_LABEL.has(type)
    ? `Stellar Index · ${TYPE_LABEL.get(type)}`
    : 'Stellar Index';
  // api.{hostname}: mirrors networks.ts's apiBaseUrl (api.stellarindex.io,
  // api.testnet.stellarindex.io, api.futurenet.stellarindex.io) — every
  // network's Pages project serves this same function under its own
  // hostname, so the origin must come from the request, not a constant.
  const apiOrigin = `https://api.${url.hostname}`;
  const sub = await liveSubline(type, rawId, apiOrigin);

  // CS-009: HTML-escape every interpolated value. Unescaped attacker input
  // reaching satori markup lets an injected `<img src=…>` trigger an
  // unauthenticated blind SSRF (satori fetches the src with no allow-list).
  const esc = (s) =>
    String(s == null ? '' : s).replace(
      /[&<>"']/g,
      (c) =>
        ({
          '&': '&amp;',
          '<': '&lt;',
          '>': '&gt;',
          '"': '&quot;',
          "'": '&#39;',
        })[c],
    );

  const html = `
    <div style="display:flex;flex-direction:column;justify-content:space-between;width:1200px;height:630px;background:#0b0f1a;color:#ffffff;padding:84px;font-family:sans-serif;">
      <div style="display:flex;font-size:30px;color:#7aa2ff;font-weight:600;">${esc(kicker)}</div>
      <div style="display:flex;flex-direction:column;">
        <div style="display:flex;font-size:76px;font-weight:700;line-height:1.05;">${esc(label)}</div>
        ${sub ? `<div style="display:flex;font-size:40px;color:#cdd6e6;margin-top:18px;">${esc(sub)}</div>` : ''}
      </div>
      <div style="display:flex;font-size:26px;color:#8a93a6;">${esc(url.hostname)}</div>
    </div>`;

  return new ImageResponse(html, {
    width: 1200,
    height: 630,
    // F087: workers-og builds its own headers object with a 'Cache-Control'
    // key, then spreads this object in afterward — plain JS object keys are
    // case-sensitive, so a differently-cased key here creates a SECOND
    // property that survives into the Headers init and gets appended
    // (not overridden), producing a doubled header value. Match the exact
    // casing so this key overrides the library's default instead.
    headers: {
      'Cache-Control': 'public, s-maxage=60, stale-while-revalidate=300',
    },
  });
}
