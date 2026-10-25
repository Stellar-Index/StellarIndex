// Browser crawl of one explorer: fails on any page that renders "Not found" (HTTP 404 or the
// client-rendered not-found shell, which curl cannot see) and on any API call a page makes that
// answers 4xx/5xx. Catches a network calling an endpoint it does not serve and an endpoint a host
// never built, neither of which site-crawl-check.sh's curl pass can see.
//
// Usage: (npm ci here first) node scripts/ci/site-crawl-browser/crawl.cjs <site> <api> [maxPages]
//   e.g. https://testnet.stellarindex.io https://api.testnet.stellarindex.io 300
// Exit 1 on any finding; one "FAIL:" line per finding.
const { chromium } = require('playwright');

const [SITE, API, MAX_ARG] = process.argv.slice(2);
if (!SITE || !API) {
  console.error('usage: crawl.cjs <site> <api> [maxPages]');
  process.exit(2);
}
const MAX_PAGES = Number(MAX_ARG || 300);
const PER_PATTERN = 3;
const SETTLE_MS = 5000;

const SEEDS = ['/', '/assets/', '/assets/usdc/', '/assets/xlm/', '/markets/', '/ledgers/', '/transactions/',
  '/operations/', '/accounts/', '/contracts/', '/protocols/', '/lending/', '/network/', '/status/', '/widgets/', '/sdk/'];

// Typed API answers (problem+json `type`) that are a page's expected reply, not a dead
// link. Each needs its reason; `page` narrows it to the pages where it is expected.
const ALLOWED_TYPES = [
  // Thin market: the price surfaces decline to publish; pages show "withheld".
  { type: 'price-withheld' },
  // A SAC is host logic with no WASM; the contract page shows the SAC panel instead.
  { type: 'contract-is-sac' },
  // A contract that never recorded supply flows; the page omits the supply panel.
  { type: 'supply-incomplete' },
  // The change worker covers configured pairs only; the chip keeps its build-time figure.
  { type: 'change-summary-not-found' },
  // A contract page probes whether the contract is also a token.
  { type: 'asset-not-found', page: /^\/contracts\// },
];

// Collapse ids so each route family is sampled PER_PATTERN times, not once per entity.
function pattern(path) {
  const segs = path.split('?')[0].split('/').filter(Boolean);
  return '/' + segs.map((s, i) => (i > 0 && /^\d+$|^[A-Za-z0-9]{20,}$|-G[A-Z0-9]{55}|~/.test(s) ? '{id}' : s)).join('/');
}

async function visit(ctx, path) {
  const page = await ctx.newPage();
  const apiErrors = [];
  const pending = [];
  page.on('response', (r) => {
    const url = r.url();
    if (!url.startsWith(API) || r.status() < 400) return;
    const rel = url.slice(API.length);
    pending.push(r.json().catch(() => ({})).then((body) => {
      const type = String(body.type || '').split('/').pop();
      if (!ALLOWED_TYPES.some((a) => a.type === type && (!a.page || a.page.test(path)))) {
        apiErrors.push(`${r.status()} ${rel}${type ? ` (${type})` : ''}`);
      }
    }));
  });
  let status = 0, title = '', heading = '', hrefs = [], error = null;
  try {
    const resp = await page.goto(SITE + path, { waitUntil: 'load', timeout: 30000 });
    status = resp ? resp.status() : 0;
    await page.waitForTimeout(SETTLE_MS);
    title = await page.title();
    heading = await page.evaluate(() => document.querySelector('main h1, main h2')?.textContent?.trim() || '');
    hrefs = await page.evaluate(() => [...document.querySelectorAll('a[href]')].map((a) => a.getAttribute('href')));
  } catch (e) {
    error = e.message.split('\n')[0].slice(0, 160);
  }
  await Promise.all(pending);
  await page.close();
  const links = new Set();
  for (const h of hrefs) {
    try {
      const u = new URL(h, SITE + path);
      if (u.origin === SITE && !/\.(png|svg|ico|json|xml|txt|css|js|pdf)$/.test(u.pathname)) links.add(u.pathname + u.search);
    } catch {
      // not a URL
    }
  }
  return { status, // A client-rendered empty state answers HTTP 200, so its heading is the only signal.
    notFound: status === 404 || /^Not found/i.test(title) || /not found$/i.test(heading), error, apiErrors, links: [...links] };
}

(async () => {
  const browser = await chromium.launch();
  const ctx = await browser.newContext();
  const seen = new Set();
  const perPattern = {};
  const queue = SEEDS.map((p) => [p, 'seed']);
  const failures = [];
  let pages = 0;
  while (queue.length && pages < MAX_PAGES) {
    const batch = [];
    while (queue.length && batch.length < 4) {
      const [path, from] = queue.shift();
      if (seen.has(path)) continue;
      seen.add(path);
      const k = pattern(path);
      perPattern[k] = (perPattern[k] || 0) + 1;
      if (perPattern[k] > PER_PATTERN) continue;
      pages++;
      batch.push(visit(ctx, path).then((r) => {
        if (r.error) failures.push(`${path} (linked from ${from}): ${r.error}`);
        if (r.notFound) failures.push(`${path} (linked from ${from}): renders Not found (HTTP ${r.status})`);
        for (const e of r.apiErrors) failures.push(`${path} (linked from ${from}): API ${e}`);
        for (const l of r.links) if (!seen.has(l)) queue.push([l, path]);
      }));
    }
    await Promise.all(batch);
  }
  await browser.close();
  for (const f of failures) console.log(`FAIL: ${f}`);
  console.log(`${SITE}: ${pages} pages, ${failures.length} finding(s)`);
  process.exit(failures.length ? 1 : 0);
})();
