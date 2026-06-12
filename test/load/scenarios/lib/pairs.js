// Representative pair fixtures used by every scenario.
//
// Mix is intentional: XLM majors dominate (matches wallet-shaped
// traffic per the Freighter RFP), with stablecoin majors and a
// couple of long-tail assets to exercise non-cached paths.
//
// CRITICAL: every `asset` and `quote` here MUST be a form that
// canonical.ParseAsset (internal/canonical/asset.go) accepts, or
// the request 400s and the scenario measures an error path instead
// of the real handler. The four accepted shapes used below are:
//
//   - `native`                       — XLM (the per-network form;
//                                       SDEX trades land under this)
//   - `crypto:<TICKER>`              — off-chain global ticker
//                                       (ADR-0014 allow-list; XLM as
//                                       `crypto:XLM` is the form CEX
//                                       trades + the aggregator VWAP
//                                       under, BTC as `crypto:BTC`)
//   - `fiat:<ISO4217>`               — off-chain reference currency
//                                       (ADR-0010 allow-list)
//   - `<CODE>-<G_ISSUER>`            — classic Stellar asset
//
// Bare codes ('XLM' aside, which ParseAsset aliases to native) like
// 'USD'/'USDC'/'AQUA' are REJECTED — they need a prefix or an
// issuer. The pre-G22-01 fixture used bare codes and silently
// measured 400 paths for ~half the mix.
//
// Issuers below are mainnet, deterministic on r1:
//   USDC  — Circle:    GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN
//   AQUA  — Aqua:      GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AB6V

const USDC = 'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';
const AQUA = 'AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AB6V';

export const PAIRS = [
  // XLM majors — both canonical forms so the alias-loop read path
  // (readPriceWithAliases) is exercised in both directions.
  { asset: 'native',     quote: 'fiat:USD',   weight: 30 },
  { asset: 'crypto:XLM', quote: 'fiat:USD',   weight: 12 },
  { asset: 'native',     quote: 'fiat:EUR',   weight: 8  },
  // Stablecoin majors quoted in fiat.
  { asset: USDC,         quote: 'fiat:USD',   weight: 14 },
  { asset: 'crypto:USDT', quote: 'fiat:USD',  weight: 7  },
  { asset: 'crypto:USDC', quote: 'fiat:USD',  weight: 5  },
  // Crypto majors quoted in fiat / against XLM.
  { asset: 'crypto:BTC', quote: 'fiat:USD',   weight: 7  },
  { asset: 'crypto:ETH', quote: 'fiat:USD',   weight: 5  },
  // Long-tail classic asset against XLM and USD.
  { asset: AQUA,         quote: 'native',     weight: 4  },
  { asset: USDC,         quote: 'native',     weight: 5  },
  { asset: 'native',     quote: 'crypto:USDT', weight: 3 },
];

const totalWeight = PAIRS.reduce((s, p) => s + p.weight, 0);

// pickWeighted returns a pair sampled by weight. Deterministic-ish
// across VUs because k6 seeds Math.random per VU.
export function pickWeighted() {
  let r = Math.random() * totalWeight;
  for (const p of PAIRS) {
    r -= p.weight;
    if (r <= 0) return p;
  }
  return PAIRS[PAIRS.length - 1];
}

// pickN returns N distinct pairs without replacement; used by
// /v1/price/batch scenarios.
export function pickN(n) {
  const pool = [...PAIRS];
  const out = [];
  for (let i = 0; i < n && pool.length; i++) {
    const idx = Math.floor(Math.random() * pool.length);
    out.push(pool.splice(idx, 1)[0]);
  }
  return out;
}

// enc URL-encodes a canonical asset id for safe inclusion in a query
// string. `fiat:USD` / `crypto:XLM` carry a colon and classic ids a
// dash — all query-safe unencoded per RFC 3986, but encoding keeps
// the suite robust against any future id shape (e.g. muxed forms).
export function enc(id) {
  return encodeURIComponent(id);
}
