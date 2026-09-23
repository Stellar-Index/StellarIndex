// Scenario 08 — network explorer (ClickHouse lake read path).
//
// Every other scenario exercises the Postgres/Redis served tier. The
// explorer routes (ADR-0038) read the ClickHouse lake instead: bloom-
// filtered skip indexes, per-account scans and wasm decompilation, all
// under an 8 s per-request deadline (explorerReadTimeout). This is the
// read class most likely to degrade under concurrency, so it gets its
// own scenario. test/load/scenario_coverage_test.go fails if an explorer
// route is mounted without a request here (or in another scenario).
//
// Fixtures are discovered from the target in setup() — recent ledgers,
// their transactions, active contracts and the wealth-ranked accounts —
// so the run measures real rows on whatever staging lake it points at.
// setup() aborts on any non-200 rather than load-testing 404 paths.
//
// Pass criteria (per endpoint, from lib/thresholds.js explorerBars):
//   - lookup (keyed by ledger / tx / account): p95 < 500 ms, p99 < 2 s
//   - scan (windowed or per-account history):  p95 < 2 s,    p99 < 8 s
//   - error rate < 0.1 %
//
// Not a release gate: a regression check for the lake read path. Run
// before any release that touches internal/storage/clickhouse queries,
// the explorer handlers, or deploy/clickhouse DDL.

import http from 'k6/http';
import { baseUrl, headers } from './lib/env.js';
import { explorerBars, rampingArrivalRate } from './lib/thresholds.js';
import { tlsWarmup } from './lib/warmup.js';

// USDC issuer (same fixture as 07): a long-lived account with deep
// history on every per-account route.
const SAMPLE_ISSUER = 'GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const SAMPLE_ASSET = `USDC-${SAMPLE_ISSUER}`;

const pick = (xs) => xs[Math.floor(Math.random() * xs.length)];

// name → k6 `endpoint` tag; cls → explorerBars key; w → relative weight.
// Account drill-down dominates, mirroring how the explorer is browsed.
const ROUTES = [
  { name: 'ledgers', cls: 'lookup', w: 6, url: () => `${baseUrl}/ledgers?limit=20` },
  { name: 'ledger-detail', cls: 'lookup', w: 6, url: (d) => `${baseUrl}/ledgers/${pick(d.seqs)}` },
  { name: 'ledger-transactions', cls: 'lookup', w: 5, url: (d) => `${baseUrl}/ledgers/${pick(d.seqs)}/transactions?limit=50` },
  { name: 'operations', cls: 'lookup', w: 4, url: (d) => `${baseUrl}/operations?ledger=${pick(d.seqs)}&limit=50` },
  { name: 'tx-detail', cls: 'lookup', w: 8, url: (d) => `${baseUrl}/tx/${pick(d.hashes)}` },
  { name: 'search', cls: 'lookup', w: 5, url: (d) => `${baseUrl}/search?q=${pick(d.hashes)}` },
  { name: 'directory', cls: 'lookup', w: 3, url: (d) => `${baseUrl}/directory?addresses=${pick(d.accounts)},${pick(d.contracts)}` },
  { name: 'account-state', cls: 'lookup', w: 8, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}` },
  { name: 'contracts', cls: 'scan', w: 3, url: () => `${baseUrl}/contracts?days=1&limit=50` },
  { name: 'contract-detail', cls: 'scan', w: 5, url: (d) => `${baseUrl}/contracts/${pick(d.contracts)}?limit=50` },
  { name: 'contract-wasm', cls: 'scan', w: 1, url: (d) => `${baseUrl}/contracts/${pick(d.contracts)}/wasm` },
  { name: 'contract-interactions', cls: 'scan', w: 3, url: (d) => `${baseUrl}/contracts/${pick(d.contracts)}/interactions?days=1&limit=50` },
  { name: 'contract-code-history', cls: 'scan', w: 2, url: (d) => `${baseUrl}/contracts/${pick(d.contracts)}/code-history` },
  { name: 'accounts', cls: 'scan', w: 3, url: () => `${baseUrl}/accounts?limit=50` },
  { name: 'accounts-stats', cls: 'scan', w: 2, url: () => `${baseUrl}/accounts/stats` },
  { name: 'account-creators', cls: 'scan', w: 2, url: () => `${baseUrl}/accounts/creators?limit=50` },
  { name: 'account-sponsors', cls: 'scan', w: 2, url: () => `${baseUrl}/accounts/sponsors?limit=50` },
  { name: 'account-transactions', cls: 'scan', w: 5, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/transactions?limit=50` },
  { name: 'account-operations', cls: 'scan', w: 5, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/operations?limit=50` },
  { name: 'account-movements', cls: 'scan', w: 5, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/movements?limit=50` },
  { name: 'account-positions', cls: 'scan', w: 3, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/positions` },
  { name: 'account-trades', cls: 'scan', w: 3, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/trades?limit=50` },
  { name: 'account-activity', cls: 'scan', w: 3, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/activity` },
  { name: 'account-graph', cls: 'scan', w: 2, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/graph?relation=created&limit=50` },
  { name: 'account-graph-history', cls: 'scan', w: 1, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/graph/history` },
  { name: 'account-graph-cohort', cls: 'scan', w: 1, url: (d) => `${baseUrl}/accounts/${pick(d.accounts)}/graph/cohort?relation=created` },
  { name: 'asset-holders', cls: 'scan', w: 2, url: () => `${baseUrl}/assets/${SAMPLE_ASSET}/holders?limit=100` },
  { name: 'network-throughput', cls: 'scan', w: 2, url: () => `${baseUrl}/network/throughput?window_days=7` },
];

const TOTAL_WEIGHT = ROUTES.reduce((s, r) => s + r.w, 0);

// One threshold per endpoint tag, generated from ROUTES so a route can
// never be sent without a bar that judges it.
const thresholds = { http_req_failed: ['rate<0.001'] };
for (const r of ROUTES) {
  thresholds[`http_req_duration{endpoint:${r.name}}`] = explorerBars[r.cls];
}

export const options = {
  scenarios: {
    // Lake scans are far heavier per request than the cached price path:
    // start from 1 rps instead of the helper's 50 and plateau at 30.
    explorer: Object.assign(
      {},
      rampingArrivalRate(
        [
          { target: 10, duration: '30s' },
          { target: 30, duration: '1m' },
          { target: 30, duration: '5m' },
          { target: 0, duration: '30s' },
        ],
        40,
      ),
      { startRate: 1 },
    ),
  },
  thresholds,
  discardResponseBodies: true,
};

function getData(path) {
  const r = http.get(`${baseUrl}${path}`, {
    headers,
    responseType: 'text',
    tags: { endpoint: 'setup' },
  });
  if (r.status !== 200) {
    throw new Error(`setup: ${path} returned ${r.status}; refusing to load-test error paths`);
  }
  return r.json('data');
}

function nonEmpty(name, xs) {
  if (!xs || xs.length === 0) {
    throw new Error(`setup: discovered no ${name} on ${baseUrl}; the lake has nothing to measure`);
  }
  return xs;
}

export function setup() {
  tlsWarmup();
  const ledgers = getData('/ledgers?limit=20').ledgers || [];
  const seqs = nonEmpty('ledgers', ledgers.map((l) => l.sequence));
  let hashes = [];
  for (const l of ledgers.filter((x) => x.tx_count > 0).slice(0, 5)) {
    const txs = getData(`/ledgers/${l.sequence}/transactions?limit=20`).transactions || [];
    hashes = hashes.concat(txs.map((t) => t.hash));
  }
  const contracts = (getData('/contracts?days=1&limit=20').contracts || []).map((c) => c.contract_id);
  const ranked = (getData('/accounts?limit=20').accounts || []).map((a) => a.account_id);
  return {
    seqs,
    hashes: nonEmpty('transactions', hashes),
    contracts: nonEmpty('contracts', contracts),
    accounts: [SAMPLE_ISSUER].concat(ranked),
  };
}

function pickRoute() {
  let r = Math.random() * TOTAL_WEIGHT;
  for (const route of ROUTES) {
    r -= route.w;
    if (r < 0) return route;
  }
  return ROUTES[ROUTES.length - 1];
}

export default function (data) {
  const route = pickRoute();
  const r = http.get(route.url(data), { headers, tags: { endpoint: route.name } });
  // A 4xx here is contract drift (a scenario measuring the 400/404 path),
  // not load; surface it as loudly as a 5xx.
  if (r.status >= 400) {
    console.warn(`${r.status} from ${route.name}`);
  }
}
