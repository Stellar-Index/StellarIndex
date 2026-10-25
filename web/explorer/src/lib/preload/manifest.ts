import { serializeJsonLd } from '@/lib/seo';

// Pages whose first-wave API calls are preloaded from an inline <head> script, so
// they start during HTML parse instead of after the JS loads and hydrates. Each
// list is pinned to what the page really fetches by manifest.test.tsx; a preload
// the page does not use costs a request, and one it misses costs nothing.

export const ACCOUNT_ID_RE = /^G[A-Z2-7]{55}$/;

/** Fetched by the console shell on every page it wraps. */
export const PRELOAD_GLOBAL = ['/v1/assets/verified', '/v1/status'];

export interface PreloadRoute {
  path: RegExp;
  /** Validates the path's captured id with the page's own rule; an invalid id preloads nothing. */
  id?: RegExp;
  /** API paths with query, `{id}` for the captured id. */
  urls: string[];
}

export const PRELOAD_ROUTES: PreloadRoute[] = [
  {
    path: /^\/$/,
    urls: [
      '/v1/assets/native',
      '/v1/assets?limit=10&order_by=volume_24h_usd_desc&include_thin=true',
      '/v1/assets?limit=50',
      '/v1/coverage',
      '/v1/diagnostics/cursors',
      '/v1/markets?limit=25&order_by=volume_24h_usd_desc',
      '/v1/markets?limit=3&order_by=volume_24h_usd_desc',
      '/v1/network/stats',
      '/v1/ohlc?base=native&quote=fiat%3AUSD&interval=15m&limit=674',
      '/v1/price/batch?asset_ids=fiat%3AEUR%2Cfiat%3AGBP%2Cfiat%3AJPY%2Cfiat%3ACHF%2Cfiat%3ACAD%2Cfiat%3AAUD&quote=fiat:USD',
      '/v1/price/batch?asset_ids=native&quote=fiat%3AUSD',
      '/v1/price?asset=native&quote=fiat%3AUSD',
      '/v1/sources?include=stats',
    ],
  },
  { path: /^\/lending\/?$/, urls: ['/v1/lending/pools'] },
  {
    path: /^\/assets\/?$/,
    urls: [
      '/v1/assets?asset_class=all&limit=100&include=sparkline7d&include_thin=true',
    ],
  },
  {
    path: /^\/contracts\/?$/,
    urls: [
      '/v1/contracts/stats',
      '/v1/contracts?days=30&limit=100',
      '/v1/sac-wrappers',
    ],
  },
  {
    path: /^\/ledgers\/?$/,
    urls: ['/v1/ledgers?limit=50', '/v1/network/throughput?window_days=30'],
  },
  {
    path: /^\/transactions\/?$/,
    urls: ['/v1/ledgers?limit=1', '/v1/network/throughput?window_days=30'],
  },
  {
    path: /^\/protocols\/?$/,
    urls: ['/v1/protocols', '/v1/sources?include=stats'],
  },
  {
    path: /^\/accounts\/?$/,
    urls: ['/v1/accounts/stats', '/v1/accounts?limit=100'],
  },
  {
    path: /^\/markets\/?$/,
    urls: [
      '/v1/markets?limit=100&order_by=volume_24h_usd_desc&include=sparkline',
    ],
  },
  {
    path: /^\/accounts\/([^/]+)\/?$/,
    id: ACCOUNT_ID_RE,
    urls: [
      '/v1/accounts/{id}',
      '/v1/accounts/{id}/claimable-balances',
      '/v1/accounts/{id}/signer-on',
      '/v1/accounts/{id}/activity',
      '/v1/accounts/{id}/movements?limit=25',
      '/v1/accounts/{id}/positions',
      '/v1/accounts/{id}/trades?limit=25',
      '/v1/accounts/{id}/graph',
      '/v1/accounts/{id}/sponsorships?limit=1',
      '/v1/accounts/{id}/transactions?limit=50',
      '/v1/accounts/{id}/operations?limit=50',
      '/v1/issuers?limit=500',
    ],
  },
];

/** Routes outside the console shell: they fetch neither the globals nor a route list. */
const SKIP = /^\/(embed|dashboard|signin)(\/|$)/;

function run(
  base: string,
  globals: string[],
  routes: [string, string, string[]][],
  skip: string,
) {
  try {
    const p = location.pathname;
    if (new RegExp(skip).test(p)) return;
    const urls = globals.slice();
    // Query strings drive sort, cursor and filter state the lists below do not encode.
    if (!location.search) {
      for (const [path, idRe, list] of routes) {
        const m = new RegExp(path).exec(p);
        if (!m) continue;
        let id = '';
        if (m[1] !== undefined) {
          try {
            id = decodeURIComponent(m[1]);
          } catch {
            break;
          }
        }
        if (idRe && !new RegExp(idRe).test(id)) break;
        for (const u of list)
          urls.push(u.split('{id}').join(encodeURIComponent(id)));
        break;
      }
    }
    for (const u of urls) {
      const l = document.createElement('link');
      l.rel = 'preload';
      l.as = 'fetch';
      l.crossOrigin = 'anonymous';
      l.href = base + u;
      document.head.appendChild(l);
    }
  } catch {
    // A preload is an optimisation; the page fetches everything itself regardless.
  }
}

/** The inline <head> script body that inserts this page's preload links. */
export function preloadScript(apiBase: string): string {
  const args = [
    // apiGet resolves every path against the base, so only its origin survives.
    new URL(apiBase).origin,
    PRELOAD_GLOBAL,
    PRELOAD_ROUTES.map((r) => [r.path.source, r.id?.source ?? '', r.urls]),
    SKIP.source,
  ];
  return `(${run.toString()}).apply(null, ${serializeJsonLd(args)});`;
}
