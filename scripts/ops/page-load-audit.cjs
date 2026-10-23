#!/usr/bin/env node
// Full end-to-end explorer page load, measured in a real browser: a page counts as loaded
// only when every request it issued (chained panels included) has settled and nothing is
// aria-busy. contract-page-audit.py models panels by hand; this reads what the page does.
//
// Usage: NODE_PATH=<dir holding playwright> node scripts/ops/page-load-audit.cjs paths.txt
//        NODE_PATH=... node scripts/ops/page-load-audit.cjs --self-test   (offline fixtures)
//   paths.txt: one explorer path per line ("/accounts/G..."); "#" starts a comment.
//   BASE (https://stellarindex.io), BUDGET_MS (1000), QUIET_MS (500), TIMEOUT_MS (30000),
//   COLD (10 cold loads per path, each in a new browser context), REPEAT (1; >1 adds warm loads
//   in the last cold context), JSON=1 for one JSON line per load, VERBOSE=1 for every request,
//   LOCAL_OUT=<web/explorer/out> to serve a local build as BASE (the API allows only BASE's origin).
// A page's time is the latest of: its last request settling, its last fetch() resolving (a
// preload settles early, the fetch that uses it later), and the last aria-busy clearing.
// Each <link rel=preload as=fetch> must be used: the page fetched its URL, and network requests for it
// equal those fetches (a preload absorbs exactly one).
// Exit code = paths whose cold p95 is over budget, or that had a load never finish or waste a preload.
'use strict';
const fs = require('fs');
const { chromium } = require('playwright');

const BASE = process.env.BASE || 'https://stellarindex.io';
const BUDGET = Number(process.env.BUDGET_MS || 1000);
const QUIET = Number(process.env.QUIET_MS || 500);
const TIMEOUT = Number(process.env.TIMEOUT_MS || 30000);
const REPEAT = Number(process.env.REPEAT || 1);
const COLD = Number(process.env.COLD || 10);
const LOCAL_OUT = process.env.LOCAL_OUT;

const TYPES = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json',
  '.svg': 'image/svg+xml', '.png': 'image/png', '.woff2': 'font/woff2', '.ico': 'image/x-icon', '.txt': 'text/plain' };

// Serves BASE's origin from a static export, as Cloudflare does: dir/index.html, then a route's
// shared shell (/accounts/{G} -> accounts/shell/index.html) for an id the export has no page for.
async function serveLocal(ctx) {
  const path = require('path');
  const origin = new URL(BASE).origin;
  await ctx.route(`${origin}/**`, (route) => {
    const p = decodeURIComponent(new URL(route.request().url()).pathname);
    const shell = p.match(/^\/([\w-]+)\/[^/]+\/?$/);
    const tries = [p, path.join(p, 'index.html'), p.replace(/\/$/, '') + '.html', shell ? `/${shell[1]}/shell/index.html` : null];
    for (const t of tries.filter(Boolean)) {
      const f = path.join(LOCAL_OUT, path.normalize(t));
      if (f.startsWith(path.resolve(LOCAL_OUT)) && fs.existsSync(f) && fs.statSync(f).isFile()) {
        return route.fulfill({ body: fs.readFileSync(f), contentType: TYPES[path.extname(f)] || 'application/octet-stream' });
      }
    }
    return route.fulfill({ status: 404, body: 'not found' });
  });
}

// Installed before any page script: records every fetch() the page makes, start and resolution.
function fetchSpy() {
  const log = [];
  window.__plFetch = log;
  const orig = window.fetch;
  window.fetch = function (input, init) {
    const url = typeof input === 'string' ? input : input instanceof URL ? input.href : input.url;
    const e = { url: new URL(url, location.href).href, start: Date.now(), end: null };
    log.push(e);
    const p = orig.call(this, input, init);
    p.then(() => { e.end = Date.now(); }, () => { e.end = Date.now(); });
    return p;
  };
}

function template(path) {
  return path
    .replace(/\/G[A-Z2-7]{55}/g, '/{G}')
    .replace(/\/C[A-Z2-7]{55}/g, '/{C}')
    .replace(/\/[0-9a-f]{64}/g, '/{hash}')
    .replace(/\/\d+(?=\/|$)/g, '/{n}')
    .replace(/^\/(assets|markets|protocols|sources|dexes|exchanges|lending|liquidity-pools|issuers)\/[^/]+/, '/$1/{id}');
}

async function loadOnce(ctx, path, base = BASE, timeout = TIMEOUT) {
  const page = await ctx.newPage();
  await page.addInitScript(fetchSpy);
  // Abort router prefetches of other routes: they also pull those routes' JS chunks, which would
  // otherwise read as this page still loading. The current page's own payload is inline in its HTML.
  await page.route((u) => u.origin === new URL(base).origin, (route) => {
    const r = route.request();
    return (r.resourceType() === 'fetch' || r.resourceType() === 'xhr') ? route.abort() : route.fallback();
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
    // API calls: fetch/xhr, plus <link rel=preload as=fetch>, which Chromium types "other".
    if (['fetch', 'xhr', 'other'].includes(r.resourceType())) {
      reqs.push({ url: u.href, status, start: r._t - t0, ms: lastSettle - r._t, end: lastSettle - t0 });
    }
  };
  // Router prefetches of other pages and long-lived streams are not part of this page's load.
  const counted = (r) => {
    if (r.resourceType() === 'eventsource' || r.resourceType() === 'websocket') return false;
    // Same-origin fetches are router payloads for other routes (link prefetch on hover/viewport).
    if ((r.resourceType() === 'fetch' || r.resourceType() === 'xhr') && new URL(r.url()).origin === new URL(base).origin) return false;
    const h = r.headers();
    return !(h['next-router-prefetch'] || h.purpose === 'prefetch' || h['sec-purpose'] || r.url().includes('_rsc='));
  };
  const network = {};
  page.on('request', (r) => {
    if (!counted(r)) return;
    r._t = Date.now();
    inflight.add(r);
    network[r.url()] = (network[r.url()] || 0) + 1;
  });
  page.on('requestfinished', async (r) => { const res = await r.response().catch(() => null); settle(r, res ? res.status() : 0); });
  page.on('requestfailed', (r) => settle(r, -1));

  let ttfb = null;
  let ok = true;
  let lastBusy = t0;
  let busyOn = false;
  let lastFetch = t0;
  let fetches = [];
  let unusedPreloads = [];
  let preloads = [];
  try {
    const resp = await page.goto(base + path, { waitUntil: 'load', timeout });
    ttfb = resp ? Math.round(resp.request().timing().responseStart) : null;
    for (;;) {
      if (Date.now() - t0 > timeout) { ok = false; break; }
      const st = await page.evaluate((origin) => ({
        busy: document.querySelectorAll('[aria-busy="true"]').length,
        // Same-origin fetches are router payloads for other routes, aborted above.
        fetches: (window.__plFetch || []).filter((f) => new URL(f.url).origin !== origin),
        preloads: [...document.querySelectorAll('link[rel="preload"][as="fetch"]')].map((l) => l.href),
      }), new URL(base).origin);
      const now = Date.now();
      // Stamped when the clear is first seen, so the time is never earlier than the real clear.
      if (st.busy) busyOn = true;
      else if (busyOn) { busyOn = false; lastBusy = now; }
      fetches = st.fetches;
      preloads = st.preloads;
      // A preload the page has not fetched yet means the render that uses it is still to come.
      unusedPreloads = st.preloads.filter((h) => !fetches.some((f) => f.url === h));
      const pending = fetches.some((f) => f.end === null) || unusedPreloads.length > 0;
      for (const f of fetches) if (f.end !== null && f.end > lastFetch) lastFetch = f.end;
      const last = Math.max(lastSettle, lastFetch, lastBusy);
      if (inflight.size === 0 && !pending && st.busy === 0 && now - last >= QUIET) break;
      await page.waitForTimeout(50);
    }
  } catch (e) {
    ok = false;
  }
  await page.close();
  const failed = reqs.filter((r) => r.status !== 404 && (r.status < 200 || r.status >= 300));
  const slowest = reqs.reduce((a, b) => (b.ms > (a ? a.ms : -1) ? b : a), null);
  const seen = {};
  for (const r of reqs) seen[r.url] = (seen[r.url] || 0) + 1;
  const dups = Object.entries(seen).filter(([, c]) => c > 1).map(([url, c]) => ({ url, count: c }));
  // A preload is used only if the page fetched its URL and every such fetch shares the network
  // requests 1:1 (the preload absorbs one fetch); an unused preload is one request with no fetch.
  const preloadBad = [...new Set(preloads)].map((url) => {
    const fetched = fetches.filter((f) => f.url === url).length;
    return { url, fetched, network: network[url] || 0 };
  }).filter((p) => p.fetched === 0 || p.network !== p.fetched);
  const firstFetch = fetches.length ? Math.min(...fetches.map((f) => f.start)) - t0 : null;
  return {
    path, template: template(path), ok: ok && failed.length === 0,
    total: Math.max(lastSettle, lastFetch, lastBusy) - t0,
    network: lastSettle - t0, fetchEnd: lastFetch - t0, busyEnd: lastBusy - t0,
    firstFetch, ttfb, n: reqs.length, slowest, failed, dups, unusedPreloads, preloads: preloads.length, preloadBad, reqs,
  };
}

function line(r, over) {
  const slow = r.slowest ? r.slowest.ms + 'ms ' + r.slowest.url : '-';
  const dup = r.dups.length ? ' dup=' + r.dups.map((d) => d.count + 'x ' + d.url).join(',') : '';
  const fail = r.failed.length ? ' failed=' + r.failed.map((f) => f.status + ' ' + f.url).join(',') : '';
  const unused = (r.unusedPreloads.length ? ' unused-preload=' + r.unusedPreloads.join(',') : '') +
    (r.preloadBad.length ? ' preload-bad=' + r.preloadBad.map((p) => `${p.url}(fetch=${p.fetched},net=${p.network})`).join(',') : '') +
    (r.preloads ? ` preloads=${r.preloads}` : '');
  return `${over ? 'FAIL' : 'ok  '} ${String(r.total).padStart(6)}ms ${r.warm ? 'warm' : 'cold'} first-fetch=${r.firstFetch ?? '-'} ` +
    `net=${r.network} fetch=${r.fetchEnd} busy=${r.busyEnd} n=${r.n} slowest=${slow}${dup}${fail}${unused}${r.ok ? '' : ' UNLOADED'} ${r.path}`;
}

// Offline fixtures served through page.route: each must report the time its page really finished.
async function selfTest(browser) {
  const base = 'https://page.fixture.test';
  const api = 'https://api.fixture.test';
  const pages = {
    // The preload settles at once; the page uses it 800 ms later and renders after that.
    '/late-preload': { min: 800, html: `<link rel="preload" as="fetch" crossorigin="anonymous" href="${api}/v1/x">
      <script>setTimeout(() => fetch('${api}/v1/x').then((r) => r.json()).then(() => { document.body.dataset.done = 1; }), 800);</script>` },
    // No requests at all; a panel stays aria-busy for 900 ms.
    '/busy': { min: 900, html: `<div id="p" aria-busy="true"></div>
      <script>setTimeout(() => document.getElementById('p').setAttribute('aria-busy', 'false'), 900);</script>` },
    // A chained fetch: the second request starts only after the first resolves.
    '/chained': { min: 400, html: `<script>setTimeout(() => fetch('${api}/v1/x').then(() => fetch('${api}/v1/y?delay=1')), 200);</script>` },
    // A preload the page fetches twice: the second fetch goes to the network, which is still a use.
    '/preload-twice': { min: 0, html: `<link rel="preload" as="fetch" crossorigin="anonymous" href="${api}/v1/x">
      <script>fetch('${api}/v1/x').then(() => fetch('${api}/v1/x'));</script>` },
    // One preload used, one never fetched: the page never counts as loaded and the unused one is named.
    '/unused-preload': { fail: true, html: `<link rel="preload" as="fetch" crossorigin="anonymous" href="${api}/v1/x">
      <link rel="preload" as="fetch" crossorigin="anonymous" href="${api}/v1/unused">
      <script>fetch('${api}/v1/x');</script>` },
  };
  let bad = 0;
  for (const [path, want] of Object.entries(pages)) {
    const ctx = await browser.newContext();
    await ctx.route(`${base}/**`, (route) => route.fulfill({ contentType: 'text/html', body: `<!doctype html><html><body>${want.html}</body></html>` }));
    await ctx.route(`${api}/**`, async (route) => {
      if (route.request().url().includes('delay=1')) await new Promise((r) => setTimeout(r, 200));
      route.fulfill({ contentType: 'application/json', headers: { 'access-control-allow-origin': '*' }, body: '{}' });
    });
    const r = await loadOnce(ctx, path, base, 3000);
    await ctx.close();
    const loaded = r.ok && !r.preloadBad.length;
    const pass = want.fail
      ? !loaded && r.preloadBad.length === 1 && r.preloadBad[0].url === `${api}/v1/unused`
      : loaded && r.total >= want.min;
    if (!pass) bad++;
    console.log(`${pass ? 'PASS' : 'FAIL'} ${path} ok=${r.ok} total=${r.total}ms want ${want.fail ? 'failure' : '>=' + want.min} ` +
      `(net=${r.network} fetch=${r.fetchEnd} busy=${r.busyEnd} preload-bad=${JSON.stringify(r.preloadBad)})`);
  }
  return bad;
}

function pct(xs, p) {
  const s = [...xs].sort((a, b) => a - b);
  return s.length ? s[Math.min(s.length - 1, Math.ceil((p / 100) * s.length) - 1)] : 0;
}

(async () => {
  const file = process.argv[2];
  if (!file) { console.error('usage: page-load-audit.cjs paths.txt | --self-test'); process.exit(2); }
  const browser = await chromium.launch();
  if (file === '--self-test') {
    const bad = await selfTest(browser);
    await browser.close();
    process.exit(bad);
  }
  const paths = fs.readFileSync(file, 'utf8').split('\n').map((l) => l.replace(/#.*/, '').trim()).filter(Boolean);
  const results = [];
  let bad = 0;
  for (const path of paths) {
    const cold = [];
    let unloaded = false;
    for (let c = 0; c < COLD; c++) {
      const ctx = await browser.newContext();
      if (LOCAL_OUT) await serveLocal(ctx);
      const warmLoads = c === COLD - 1 ? REPEAT : 1;
      for (let i = 0; i < warmLoads; i++) {
        const r = await loadOnce(ctx, path);
        r.warm = i > 0;
        results.push(r);
        if (!r.warm) { cold.push(r.total); unloaded = unloaded || !r.ok || r.preloadBad.length > 0; }
        const out = { ...r };
        if (!process.env.VERBOSE) delete out.reqs;
        if (process.env.JSON) console.log(JSON.stringify(out));
        else console.log(line(r, !r.ok || r.preloadBad.length > 0 || r.total > BUDGET));
      }
      await ctx.close();
    }
    if (unloaded || pct(cold, 95) > BUDGET) bad++;
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
