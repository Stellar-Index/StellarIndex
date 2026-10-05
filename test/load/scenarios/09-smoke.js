// Scenario 09 — smoke. One pass over the read routes the other
// scenarios drive, 1 VU x 1 iteration. Fails on any http_req_failed,
// so a scenario that drifted onto a 4xx/5xx path is caught in seconds.
// Not a load test: no latency bar.

import http from 'k6/http';
import { baseUrl, headers } from './lib/env.js';
import { PAIRS, enc } from './lib/pairs.js';
import { sla } from './lib/thresholds.js';
import { tlsWarmup } from './lib/warmup.js';

export const options = {
  vus: 1,
  iterations: 1,
  thresholds: sla.smoke,
  discardResponseBodies: true,
};

export function setup() {
  tlsWarmup();
}

export default function () {
  const pair = PAIRS[0];
  const paths = [
    '/healthz',
    `/price?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}`,
    '/assets?limit=10',
    '/issuers?limit=10',
    '/markets?limit=10',
    '/diagnostics/cursors',
  ];
  for (const p of paths) {
    http.get(`${baseUrl}${p}`, { headers, tags: { endpoint: 'smoke' } });
  }
}
