// Shared static-param generation for the /convert/[from]/[to] hub-and-spoke
// matrix. Used by BOTH the route's generateStaticParams and the sitemap, so the
// set of pre-rendered convert pages and the sitemap entries never drift (a
// sitemap URL that 404s is a Search Console error). See the route page for the
// SEO rationale (top-20 fiat majors hub all long-tail conversions).

/**
 * Top-20 fiat majors that hub all the long-tail conversions — the currencies
 * people search "from" and "to". Keep in sync with the route's intent; this is
 * the single source of truth for both the page and the sitemap.
 */
const HUB_TICKERS = [
  'USD',
  'EUR',
  'GBP',
  'JPY',
  'CHF',
  'CAD',
  'AUD',
  'CNY',
  'INR',
  'BRL',
  'MXN',
  'ZAR',
  'NZD',
  'SGD',
  'HKD',
  'SEK',
  'NOK',
  'KRW',
  'TRY',
  'PLN',
];

/** Ticker → canonical asset id for the verified Stellar assets on /convert. */
export type ConvertAssetIds = Readonly<Record<string, string>>;

/** A verified Stellar credit asset the /convert routes offer. */
export interface ConvertAsset {
  ticker: string;
  /** `<code>-<issuer>` from the catalogue; prices are read by this, never by ticker. */
  assetId: string;
  name: string;
  slug: string;
}

/** Caps the asset × hub pages the static export bakes. */
export const MAX_CONVERT_ASSETS = 50;

/**
 * The verified Stellar credit assets /convert offers, in catalogue order.
 * Only rows carrying a catalogue (code, issuer) id qualify; an asset whose
 * upper-cased ticker collides with XLM, a fiat ticker or an earlier asset is
 * dropped, because the URL segment is the upper-cased ticker.
 */
export function convertAssets(
  rows: readonly {
    ticker: string;
    name?: string;
    slug?: string;
    asset_id?: string;
    issuer?: string;
  }[],
  fiat: readonly string[],
): ConvertAsset[] {
  const taken = new Set(['XLM', ...fiat.map((t) => t.toUpperCase())]);
  const out: ConvertAsset[] = [];
  for (const row of rows) {
    if (out.length >= MAX_CONVERT_ASSETS) break;
    if (!row.asset_id || !row.issuer || !row.ticker) continue;
    const key = row.ticker.toUpperCase();
    if (taken.has(key)) continue;
    taken.add(key);
    out.push({
      ticker: row.ticker,
      assetId: row.asset_id,
      name: row.name ?? row.ticker,
      slug: row.slug ?? row.asset_id,
    });
  }
  return out;
}

/** The API asset id for a /convert ticker: XLM is `native`, a verified asset its catalogue id, the rest fiat. */
export function convertAssetId(
  ticker: string,
  ids: ConvertAssetIds = {},
): string {
  if (ticker === 'XLM') return 'native';
  return Object.hasOwn(ids, ticker) ? ids[ticker] : `fiat:${ticker}`;
}

/**
 * The priced read behind a {from}/{to} rate. Fiat is only priced as the
 * quote, so fiat → XLM or a Stellar asset reads that asset in {from} and the
 * caller takes 1/price.
 */
export function convertQuery(
  from: string,
  to: string,
  ids: ConvertAssetIds = {},
): { asset: string; quote: string; invert: boolean } {
  const asset = convertAssetId(from, ids);
  const quote = convertAssetId(to, ids);
  return asset.startsWith('fiat:') && !quote.startsWith('fiat:')
    ? { asset: quote, quote: asset, invert: true }
    : { asset, quote, invert: false };
}

/**
 * One direction only: every verified asset → every served hub. The `from`
 * segment is the upper-cased ticker, the form the /convert Function
 * canonicalises to.
 */
export function buildAssetConvertParams(
  assets: readonly ConvertAsset[],
  tickers: readonly string[],
): { from: string; to: string }[] {
  const served = new Set(tickers);
  const hubs = HUB_TICKERS.filter((t) => served.has(t));
  return assets.flatMap((a) =>
    hubs.map((to) => ({ from: a.ticker.toUpperCase(), to })),
  );
}

/**
 * Hub-and-spoke: every hub × every ticker (forward) + every non-hub ticker ×
 * every hub (reverse). Pure — given the same ticker list, returns the same
 * pairs the route pre-renders.
 */
export function buildConvertParams(
  tickers: string[],
): { from: string; to: string }[] {
  const out: { from: string; to: string }[] = [];
  // A hub the catalogue does not serve would bake a page whose identity read
  // fails the export, so hubs are only ever drawn from the served tickers.
  const served = new Set(tickers);
  const hubs = HUB_TICKERS.filter((t) => served.has(t));
  const hubSet = new Set(hubs);
  // Pass 1: every hub × every ticker (forward).
  for (const from of hubs) {
    for (const to of tickers) {
      if (from === to) continue;
      out.push({ from, to });
    }
  }
  // Pass 2: every non-hub ticker → every hub (reverse); hub×hub already in pass 1.
  for (const from of tickers) {
    if (hubSet.has(from)) continue;
    for (const to of hubs) {
      if (from === to) continue;
      out.push({ from, to });
    }
  }
  return out;
}
