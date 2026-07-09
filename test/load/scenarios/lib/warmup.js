// Pre-warmup helpers.
//
// The Redis hot-path scenario must not measure cold-cache requests
// (every miss becomes a Timescale full query — measures the wrong
// thing). Each scenario's setup() calls warmPriceCache once so the
// first measured request hits a populated cache.
//
// Also hits /v1/healthz once per VU on iteration 0 to absorb the
// TLS handshake into a non-measured request — keeps p95 honest.
//
// Both functions used to fire-and-forget the warmup requests: neither
// checked the response status, so a totally unreachable/misconfigured
// K6_TARGET (or an auth failure) silently produced a "successful"
// setup() and the scenario burned its full multi-minute duration
// measuring nothing useful (audit-2026-06-14 A20, harness
// error-swallowing residual). k6 aborts the whole run when setup()
// throws, so:
//   - tlsWarmup is the first request of every scenario's setup() — a
//     non-2xx here means the target itself is down/misconfigured, so
//     it throws (propagate to the run's exit code) rather than
//     wasting the run.
//   - warmPriceCache is best-effort PER PAIR — one pair legitimately
//     missing from cache shouldn't abort a whole scenario, so a
//     per-pair miss is a logged warning, not fatal. But if EVERY pair
//     fails, that's the same "target is broken" signal as tlsWarmup
//     and is treated the same way.

import http from 'k6/http';
import { baseUrl, headers } from './env.js';
import { PAIRS, enc } from './pairs.js';

function isOK(r) {
  return r.status >= 200 && r.status < 300;
}

export function warmPriceCache() {
  let failures = 0;
  for (const p of PAIRS) {
    const r = http.get(`${baseUrl}/price?asset=${enc(p.asset)}&quote=${enc(p.quote)}`, {
      headers,
      tags: { endpoint: 'warmup' },
    });
    if (!isOK(r)) {
      failures++;
      // Explicit, non-fatal tolerance: a single pair missing from
      // cache degrades that pair's first measured request but isn't
      // worth aborting the run over.
      console.warn(
        `warmPriceCache: ${p.asset}/${p.quote} returned ${r.status} ` +
        `(error=${r.error || 'n/a'}) — that pair's cache may still be cold`,
      );
    }
  }
  if (failures === PAIRS.length) {
    throw new Error(
      `warmPriceCache: all ${PAIRS.length} warmup requests failed against ` +
      `${baseUrl} — target is likely unreachable or misconfigured; aborting ` +
      'before burning the full run measuring nothing.',
    );
  }
}

export function tlsWarmup() {
  const r = http.get(`${baseUrl}/healthz`, {
    headers,
    tags: { endpoint: 'warmup' },
  });
  if (!isOK(r)) {
    throw new Error(
      `tlsWarmup: GET ${baseUrl}/healthz returned ${r.status} ` +
      `(error=${r.error || 'n/a'}) — target is unreachable or unhealthy; ` +
      'aborting before burning the full run measuring nothing.',
    );
  }
}
