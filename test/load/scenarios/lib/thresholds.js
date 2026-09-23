// Shared SLA thresholds. Every scenario imports from here so the
// pass/fail bar is defined once and matches:
//   - API SLA targets: p95 ≤ 200 ms
//   - ADR-0009 multi-window SLO: 99.9 % availability
//
// Endpoint tags (`endpoint:price` etc.) are set by each scenario
// via `tags: { endpoint: '...' }` on the http call so per-endpoint
// p95 can be asserted independently.

export const sla = {
  priceHotPath: {
    'http_req_duration{endpoint:price}': ['p(95)<200', 'p(99)<500'],
    'http_req_failed':                   ['rate<0.001'],
  },
  vwapTwap: {
    'http_req_duration{endpoint:vwap}':  ['p(95)<200', 'p(99)<500'],
    'http_req_duration{endpoint:twap}':  ['p(95)<200', 'p(99)<500'],
    'http_req_failed':                   ['rate<0.001'],
  },
  history: {
    'http_req_duration{endpoint:history}':           ['p(95)<200', 'p(99)<500'],
    'http_req_duration{endpoint:since-inception}':   ['p(95)<1000', 'p(99)<2000'],
    'http_req_failed':                               ['rate<0.001'],
  },
  batch: {
    'http_req_duration{endpoint:batch}':  ['p(95)<500', 'p(99)<1000'],
    'http_req_failed':                    ['rate<0.001'],
  },
  streaming: {
    // SSE clients are long-lived; we measure first-event latency
    // via a custom Trend, not http_req_duration.
    'sse_first_event_ms': ['p(99)<1000'],
    'http_req_failed':    ['rate<0.001'],
  },
  mixed: {
    // The canonical proof: weighted mix p95 ≤ 200 ms, 99.9 % success.
    'http_req_duration': ['p(95)<200', 'p(99)<500'],
    'http_req_failed':   ['rate<0.001'],
  },
  spike: {
    // The 10× burst should not error; latency excused mid-spike,
    // recovery asserted out-of-band by the runbook.
    'http_req_failed': ['rate<0.005'],
  },
  catalogue: {
    // Catalogue endpoints — same SLA bar as other read
    // surfaces. /v1/markets does a GROUP BY across the 14-day
    // chunk window so we allow a slightly looser p99 than
    // single-key lookups; everything else stays under the
    // standard 200 ms / 500 ms gate.
    'http_req_duration{endpoint:assets}':             ['p(95)<200', 'p(99)<500'],
    'http_req_duration{endpoint:issuers}':            ['p(95)<200', 'p(99)<500'],
    'http_req_duration{endpoint:issuer-detail}':      ['p(95)<200', 'p(99)<500'],
    'http_req_duration{endpoint:markets}':            ['p(95)<300', 'p(99)<1000'],
    'http_req_duration{endpoint:cursors}':            ['p(95)<200', 'p(99)<500'],
    'http_req_failed':                                ['rate<0.001'],
  },
};

// Explorer (ADR-0038) latency bars, applied per endpoint by
// 08-explorer-lake.js. These routes read the ClickHouse lake, not the
// cached price tier, so the 200 ms Freighter target does not apply.
// The scan p99 equals explorerReadTimeout (8 s): past it requests are
// being cut off, not served. Provisional until a measured staging run.
export const explorerBars = {
  lookup: ['p(95)<500', 'p(99)<2000'],
  scan:   ['p(95)<2000', 'p(99)<8000'],
};

// Common executor shape — RPS-controlled per the design note Q4.
// Scenarios override stages but inherit the executor type.
export function rampingArrivalRate(stages, preAllocatedVUs = 100) {
  return {
    executor: 'ramping-arrival-rate',
    startRate: 50,
    timeUnit: '1s',
    preAllocatedVUs,
    maxVUs: preAllocatedVUs * 4,
    stages,
  };
}
