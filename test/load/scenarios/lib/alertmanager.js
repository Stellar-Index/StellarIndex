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

import http from 'k6/http';

const ENV = (typeof __ENV !== 'undefined') ? __ENV : {};
const url = (ENV.ALERTMANAGER_URL || '').replace(/\/$/, '');
// Default matchers MUST be the real deployed alert names (the `alert:`
// values in configs/prometheus/rules.r1/api.yml +
// deploy/monitoring/rules/api.yml). The old defaults
// (APIHighLatencyP95 / APIHighErrorRate) matched NO alert, so the
// 99-spike silence was a silent no-op and on-call paged during the
// planned burst (audit-2026-06-14 A20). Cover the latency + error-rate
// alerts the 10× spike legitimately trips.
const matchers = (ENV.ALERTMANAGER_SILENCE_MATCHERS ||
  'alertname=stellarindex_api_latency_p95_high,' +
  'alertname=stellarindex_api_latency_p99_high,' +
  'alertname=stellarindex_api_error_rate_high,' +
  'alertname=stellarindex_api_error_rate_critical').split(',');
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
