#!/usr/bin/env node
// Full end-to-end explorer page load, measured in a real browser: a page counts as loaded
// only when every request it issued (chained panels included) has settled and nothing is
// aria-busy. contract-page-audit.py models panels by hand; this reads what the page does.
//
// Usage: NODE_PATH=<dir holding playwright> node scripts/ops/page-load-audit.cjs paths.txt
//   paths.txt: one explorer path per line ("/accounts/G..."); "#" starts a comment.
//   BASE (https://stellarindex.io), BUDGET_MS (1000), QUIET_MS (500), TIMEOUT_MS (30000),
//   REPEAT (1; >1 adds warm loads in the same context), JSON=1 for one JSON line per load.
// Exit code = pages over budget + pages that never finished loading.
'use strict';
const fs = require('fs');
const { chromium } = require('playwright');

const BASE = process.env.BASE || 'https://stellarindex.io';
const BUDGET = Number(process.env.BUDGET_MS || 1000);
const QUIET = Number(process.env.QUIET_MS || 500);
const TIMEOUT = Number(process.env.TIMEOUT_MS || 30000);
const REPEAT = Number(process.env.REPEAT || 1);

function template(path) {
  return path
    .replace(/\/G[A-Z2-7]{55}/g, '/{G}')
    .replace(/\/C[A-Z2-7]{55}/g, '/{C}')
    .replace(/\/[0-9a-f]{64}/g, '/{hash}')
    .replace(/\/\d+(?=\/|$)/g, '/{n}')
    .replace(/^\/(assets|markets|protocols|sources|dexes|exchanges|lending|liquidity-pools|issuers)\/[^/]+/, '/$1/{id}');
}

async function loadOnce(ctx, path) {
  const page = await ctx.newPage();
  // Abort router prefetches of other routes: they also pull those routes' JS chunks, which would
  // otherwise read as this page still loading. The current page's own payload is inline in its HTML.
  await page.route((u) => u.origin === new URL(BASE).origin, (route) => {
    const r = route.request();
    return (r.resourceType() === 'fetch' || r.resourceType() === 'xhr') ? route.abort() : route.continue();
  });
  const t0 = Date.now();
  const inflight = new Set();
  const reqs = [];
  let lastSettle = t0;
  const settle = (r, status) => {
    if (!inflight.delete(r)) return;
    lastSettle = Date.now();
    const u = new URL(r.url());
    if (process.env.VERBOSE) console.log(`  ${String(r._t - t0).padStart(5)} +${String(lastSettle - r._t).padStart(5)} ${r.resourceType()} ${status} ${u.host}${u.pathname}`);
    if (r.resourceType() === 'fetch' || r.resourceType() === 'xhr') {
      reqs.push({ url: u.host + u.pathname + u.search, status, ms: lastSettle - r._t, end: lastSettle - t0 });
    }
  };
  // Router prefetches of other pages and long-lived streams are not part of this page's load.
  const counted = (r) => {
    if (r.resourceType() === 'eventsource' || r.resourceType() === 'websocket') return false;
    // Same-origin fetches are router payloads for other routes (link prefetch on hover/viewport).
    if ((r.resourceType() === 'fetch' || r.resourceType() === 'xhr') && new URL(r.url()).origin === new URL(BASE).origin) return false;
    const h = r.headers();
    return !(h['next-router-prefetch'] || h.purpose === 'prefetch' || h['sec-purpose'] || r.url().includes('_rsc='));
  };
  page.on('request', (r) => { if (!counted(r)) return; r._t = Date.now(); inflight.add(r); });
  page.on('requestfinished', async (r) => { const res = await r.response().catch(() => null); settle(r, res ? res.status() : 0); });
  page.on('requestfailed', (r) => settle(r, -1));

  let ttfb = null;
  let ok = true;
  try {
    const resp = await page.goto(BASE + path, { waitUntil: 'load', timeout: TIMEOUT });
    ttfb = resp ? Math.round(resp.request().timing().responseStart) : null;
    for (;;) {
      if (Date.now() - t0 > TIMEOUT) { ok = false; break; }
      const busy = await page.locator('[aria-busy="true"]').count();
      if (process.env.VERBOSE && busy && !page._busyLogged) { page._busyLogged = 1; console.log('  busy at', Date.now() - t0); }
      if (inflight.size === 0 && busy === 0 && Date.now() - lastSettle >= QUIET) break;
      await page.waitForTimeout(50);
    }
  } catch (e) {
    ok = false;
  }
  await page.close();
  const failed = reqs.filter((r) => r.status !== 404 && (r.status < 200 || r.status >= 300));
  const slowest = reqs.reduce((a, b) => (b.ms > (a ? a.ms : -1) ? b : a), null);
  return { path, template: template(path), ok: ok && failed.length === 0, total: lastSettle - t0, ttfb, n: reqs.length, slowest, failed };
}

function pct(xs, p) {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.min(s.length - 1, Math.ceil((p / 100) * s.length) - 1)] : 0;
}

(async () => {
  const file = process.argv[2];
  if (!file) { console.error('usage: page-load-audit.cjs paths.txt'); process.exit(2); }
  const paths = fs.readFileSync(file, 'utf8').split('\n').map((l) => l.replace(/#.*/, '').trim()).filter(Boolean);
  const browser = await chromium.launch();
  const results = [];
  let bad = 0;
  for (const path of paths) {
    const ctx = await browser.newContext();
    for (let i = 0; i < REPEAT; i++) {
      const r = await loadOnce(ctx, path);
      r.warm = i > 0;
      results.push(r);
      const over = !r.ok || r.total > BUDGET;
      if (!r.warm && over) bad++;
      if (process.env.JSON) console.log(JSON.stringify(r));
      else console.log(`${over ? 'FAIL' : 'ok  '} ${String(r.total).padStart(6)}ms ${r.warm ? 'warm' : 'cold'} n=${r.n} slowest=${r.slowest ? r.slowest.ms + 'ms ' + r.slowest.url : '-'}${r.failed.length ? ' failed=' + r.failed.map((f) => f.status + ' ' + f.url).join(',') : ''}${r.ok ? '' : ' UNLOADED'} ${path}`);
    }
    await ctx.close();
  }
  await browser.close();
  const byT = {};
  for (const r of results.filter((x) => !x.warm)) (byT[r.template] = byT[r.template] || []).push(r.total);
  console.log('\ntemplate  n  p50  p95  max  (cold, ms)');
  for (const [t, xs] of Object.entries(byT).sort((a, b) => pct(b[1], 95) - pct(a[1], 95))) {
    console.log(`${t}  ${xs.length}  ${pct(xs, 50)}  ${pct(xs, 95)}  ${Math.max(...xs)}`);
  }
  process.exit(Math.min(bad, 125));
})();
