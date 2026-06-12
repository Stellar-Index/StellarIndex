// Scenario 06 — mixed-realistic proof scenario.
//
// THIS IS THE FREIGHTER SLA PROOF (Task #77). All other scenarios
// stress one endpoint shape; this one runs the weighted blend
// from the design note §Traffic shape so the resulting p95 / p99
// / error-rate numbers reflect what a real production day looks
// like.
//
// Pass criteria:
//   - p95 < 200 ms across the full mix
//   - p99 < 500 ms across the full mix
//   - error rate < 0.1 % (99.9 % SLO; ADR-0009)
//   - sustained 10 min minimum
//
// Traffic shape (per design note):
//   60% /v1/price (single)
//   15% /v1/price/batch
//   10% /v1/price/tip
//    6% /v1/vwap
//    4% /v1/history
//    3% /v1/twap
//    1% /v1/observations/stream (SSE)
//    1% /v1/oracle/lastprice (SEP-40)
//
// After this run passes, the operator generates the SLA proof
// markdown at docs/operations/sla-proof-<YYYY-MM-DD>.md from the
// Prometheus run window + Grafana snapshot.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { baseUrl, headers } from './lib/env.js';
import { pickWeighted, pickN, enc } from './lib/pairs.js';
import { sla, rampingArrivalRate } from './lib/thresholds.js';
import { tlsWarmup, warmPriceCache } from './lib/warmup.js';

const HISTORY_WINDOWS = ['1h', '6h', '24h', '7d'];
// /v1/history reads `granularity=`, not `resolution=` (history.go).
const HISTORY_GRANULARITIES = ['5m', '1h', '1d'];
const VWAP_WINDOWS = ['5m', '15m', '1h', '24h'];

// Single quote pinned for the batch sub-mix — POST /v1/price/batch
// applies one quote to every asset_id (price.go). fiat:USD is not in
// the asset fixture, so no identity-400 aborts the batch.
const BATCH_QUOTE = 'fiat:USD';

export const options = {
  scenarios: {
    mixed: rampingArrivalRate([
      { target: 100, duration: '30s' },
      { target: 300, duration: '2m'  },
      { target: 300, duration: '10m' },  // the soak — proves the SLO
      { target: 0,   duration: '30s' },
    ], 100),
  },
  thresholds: sla.mixed,
  discardResponseBodies: true,
};

export function setup() {
  tlsWarmup();
  warmPriceCache();
}

function pickEndpoint() {
  const r = Math.random() * 100;
  if (r < 60)  return 'price';
  if (r < 75)  return 'batch';
  if (r < 85)  return 'price-tip';
  if (r < 91)  return 'vwap';
  if (r < 95)  return 'history';
  if (r < 98)  return 'twap';
  if (r < 99)  return 'stream';
  return 'oracle-lastprice';
}

export default function () {
  const ep = pickEndpoint();
  const pair = pickWeighted();
  let r;

  switch (ep) {
    case 'price':
      r = http.get(
        `${baseUrl}/price?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}`,
        { headers, tags: { endpoint: 'price' } },
      );
      break;

    case 'price-tip':
      r = http.get(
        `${baseUrl}/price/tip?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}`,
        { headers, tags: { endpoint: 'price' } },
      );
      break;

    case 'batch': {
      const picks = pickN(10);
      const body = JSON.stringify({
        asset_ids: picks.map((p) => p.asset),
        quote: BATCH_QUOTE,
      });
      r = http.post(`${baseUrl}/price/batch`, body, {
        headers: { ...headers, 'Content-Type': 'application/json' },
        tags: { endpoint: 'batch' },
      });
      break;
    }

    case 'vwap': {
      const w = VWAP_WINDOWS[Math.floor(Math.random() * VWAP_WINDOWS.length)];
      r = http.get(
        `${baseUrl}/vwap?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}&window=${w}`,
        { headers, tags: { endpoint: 'vwap' } },
      );
      break;
    }

    case 'twap': {
      const w = VWAP_WINDOWS[Math.floor(Math.random() * VWAP_WINDOWS.length)];
      r = http.get(
        `${baseUrl}/twap?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}&window=${w}`,
        { headers, tags: { endpoint: 'twap' } },
      );
      break;
    }

    case 'history': {
      const w = HISTORY_WINDOWS[Math.floor(Math.random() * HISTORY_WINDOWS.length)];
      const g = HISTORY_GRANULARITIES[Math.floor(Math.random() * HISTORY_GRANULARITIES.length)];
      r = http.get(
        `${baseUrl}/history?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}&window=${w}&granularity=${g}`,
        { headers, tags: { endpoint: 'history' } },
      );
      break;
    }

    case 'stream':
      // SSE is sampled in the mix at 1 % but only as a connection
      // open — the soak quickly accumulates lingering clients.
      // Holding open inside this iteration would block all VUs;
      // instead we measure the connection-accept latency only.
      r = http.get(
        `${baseUrl}/observations/stream?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}`,
        { headers: { ...headers, 'Accept': 'text/event-stream' }, tags: { endpoint: 'stream' }, timeout: '5s' },
      );
      break;

    case 'oracle-lastprice':
      // /v1/oracle/lastprice (SEP-40) reads `asset` only; quote is
      // implicit USD. Extra quote= is ignored harmlessly.
      r = http.get(
        `${baseUrl}/oracle/lastprice?asset=${enc(pair.asset)}`,
        { headers, tags: { endpoint: 'oracle' } },
      );
      break;
  }

  if (r) {
    check(r, {
      'status 2xx': (r) => r.status >= 200 && r.status < 300,
    });
  }
  sleep(0.05);
}
