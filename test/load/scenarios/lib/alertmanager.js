// AlertManager-silence helpers.
//
// The 99-spike scenario will legitimately trip
// `stellarindex_api_latency_p95_high` mid-spike. Without a silence,
// on-call gets paged for the planned spike — design note §6.
//
// silenceForRun posts a silence to AlertManager covering the
// expected run window plus a buffer; clearSilence removes it on
// teardown so a real post-run regression still pages.
//
// Both functions are no-ops when ALERTMANAGER_URL is unset, so
// scenarios run fine against a staging environment that doesn't
// have AlertManager wired (the silence is a courtesy, not a
// correctness invariant).
//
// SCOPE OF THE DEFAULT SILENCE — read before adding an alertname:
//
//   Silence ONLY alerts the load run is *documented* to legitimately
//   trip as a side effect of the ramp shape, never alerts that
//   measure the thing the run is supposed to prove. 99-spike's own
//   pass criteria (test/load/scenarios/lib/thresholds.js `sla.spike`:
//   `http_req_failed: rate<0.005`) GATES the error rate — the run
//   fails if 5xx exceeds 0.5%. Latency is explicitly "excused mid-spike"
//   (same file, comment on `spike`) — that's the only excused axis.
//   So the default set below is latency-only:
//     - stellarindex_api_latency_p95_high (ticket) — design note §6,
//       the documented legitimate trip.
//     - stellarindex_api_latency_p99_high (ticket) — same axis, same
//       10x-burst cause; ticket severity, not paging.
//   error_rate_high/error_rate_critical are deliberately NOT silenced:
//   error_rate_critical is `severity: page` (SEV-1) in
//   configs/prometheus/rules.r1/api.yml — if the spike genuinely pushes
//   5xx that high, that IS a real page, not planned-burst noise, and
//   on-call must see it. (audit-2026-06-14 R-A20-1 follow-up: the
//   first fix for the "matches no alert" typo bug over-corrected by
//   also silencing both error-rate alerts, which masked exactly the
//   failure this scenario's own threshold is designed to catch — the
//   inverse HIGH failure mode of the original bug.)
//
// scripts/ci/lint-docs.sh §17 enforces both invariants in CI: every
// default matcher must resolve to a real `alert:` name in both rule
// dirs (guards the "matches no alert, silent no-op" class), and none
// may carry `severity: page` (guards the "silences a real SEV-1" class
// this comment describes). Manual dry-run verification (no k6 needed):
//
//   amtool silence add --alertmanager.url=$ALERTMANAGER_URL \
//     alertname=stellarindex_api_latency_p95_high \
//     alertname=stellarindex_api_latency_p99_high \
//     --author=manual-dry-run --comment='dry run' --duration=1m
//   amtool silence query --alertmanager.url=$ALERTMANAGER_URL
//   # then expire it immediately:
//   amtool silence expire --alertmanager.url=$ALERTMANAGER_URL <id>

import http from 'k6/http';

const ENV = (typeof __ENV !== 'undefined') ? __ENV : {};
const url = (ENV.ALERTMANAGER_URL || '').replace(/\/$/, '');
// Default matchers MUST be the real deployed alert names (the `alert:`
// values in configs/prometheus/rules.r1/api.yml +
// deploy/monitoring/rules/api.yml) AND must be restricted to alerts
// this scenario is documented to legitimately trip — see the SCOPE
// comment above. Do not add an alertname here without updating that
// comment and confirming (lint-docs.sh §17 will check) it isn't
// `severity: page`.
const matchers = (ENV.ALERTMANAGER_SILENCE_MATCHERS ||
  'alertname=stellarindex_api_latency_p95_high,' +
  'alertname=stellarindex_api_latency_p99_high').split(',');
const author = ENV.ALERTMANAGER_SILENCE_AUTHOR || 'k6-load-test';

// silenceForRun creates a silence covering durationMinutes from
// now, returns the silence ID for later removal.
export function silenceForRun(durationMinutes, comment) {
  if (!url) return null;
  const startsAt = new Date().toISOString();
  const endsAt = new Date(Date.now() + durationMinutes * 60 * 1000).toISOString();

  const payload = {
    matchers: matchers.map((m) => {
      const [name, value] = m.split('=');
      return { name, value, isRegex: false };
    }),
    startsAt,
    endsAt,
    createdBy: author,
    comment: comment || 'planned k6 load test',
  };

  const r = http.post(`${url}/api/v2/silences`, JSON.stringify(payload), {
    headers: { 'Content-Type': 'application/json' },
    tags: { endpoint: 'alertmanager-silence' },
  });
  if (r.status !== 200) {
    console.warn(`alertmanager silence create returned ${r.status}: ${r.body}`);
    return null;
  }
  try {
    return JSON.parse(r.body).silenceID;
  } catch (e) {
    return null;
  }
}

export function clearSilence(silenceID) {
  if (!url || !silenceID) return;
  const r = http.del(`${url}/api/v2/silence/${silenceID}`, null, {
    tags: { endpoint: 'alertmanager-silence' },
  });
  if (r.status >= 400) {
    console.warn(`alertmanager silence delete ${silenceID} returned ${r.status}`);
  }
}
