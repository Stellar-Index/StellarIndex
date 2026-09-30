// Scenario 05 — /v1/price/stream + /v1/observations/stream (SSE).
//
// What it stresses: the streaming hub's connection-accept path,
// per-client buffer, and last-event-id resume contract.
//
// Pass criteria: 99 % of clients receive their first event within
// 1 s of subscribe (sse_first_event_ms p99 < 1000 in
// lib/thresholds.js), and 99.9 % of subscribes deliver a first event
// before the client timeout ends them (sse_subscribe_ok).
//
// Executor is constant-VUs not ramping-arrival-rate — long-lived
// SSE connections need a fixed concurrent count, not an RPS curve.
// Each VU subscribes once, records first-event latency, then
// holds the connection open for the rest of the iteration.
//
// Edge case (design note §5): SSE clients linger. At 200 VUs the
// scenario ends with up to 200 lingering connections — the hub's
// shutdown path is exercised on scenario teardown.

import http from 'k6/http';
import { check } from 'k6';
import { Rate, Trend } from 'k6/metrics';
import { baseUrl, apiKey } from './lib/env.js';
import { pickWeighted, enc } from './lib/pairs.js';
import { sla } from './lib/thresholds.js';

const firstEvent = new Trend('sse_first_event_ms');
const subscribeOk = new Rate('sse_subscribe_ok');

// k6 error code for "request timeout" — the expected end of every subscribe.
const K6_REQUEST_TIMEOUT = 1050;

export const options = {
  scenarios: {
    streaming: {
      executor: 'ramping-vus',
      startVUs: 0,
      stages: [
        { target: 50,  duration: '30s' },
        { target: 200, duration: '2m'  },
        { target: 200, duration: '5m'  },
        { target: 0,   duration: '30s' },
      ],
      gracefulStop: '60s',
    },
  },
  thresholds: sla.streaming,
};

export default function () {
  const pair = pickWeighted();
  const url = `${baseUrl}/observations/stream?asset=${enc(pair.asset)}&quote=${enc(pair.quote)}`;

  // k6's http.get on a streaming endpoint blocks until server
  // closes or timeout; the server never closes, so every request
  // ends in the 30s client timeout. We measure first-event latency
  // by checking response.timings.waiting, which is the
  // time-to-first-byte — for SSE that's exactly the first event
  // arrival.
  const r = http.get(url, {
    headers: { 'X-API-Key': apiKey, 'Accept': 'text/event-stream' },
    tags: { endpoint: 'stream' },
    timeout: '30s',
  });

  // A timed-out request reports status 0, so a healthy subscribe is
  // recognised by its timeout code plus a first byte, not by status.
  // waiting is non-zero even when no byte arrived; receiving is not.
  const opened = r.timings.receiving > 0 &&
    (r.error_code === K6_REQUEST_TIMEOUT || (r.status === 200 && !r.error_code));
  // A failed subscribe has no first event, so its waiting is not a first-event latency.
  if (opened) {
    firstEvent.add(r.timings.waiting);
  }
  subscribeOk.add(opened);

  check(r, {
    'first event within 30s': () => opened,
  });
}
