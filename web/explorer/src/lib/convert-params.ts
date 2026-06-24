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
export const HUB_TICKERS = [
  'USD', 'EUR', 'GBP', 'JPY', 'CHF', 'CAD', 'AUD', 'CNY',
  'INR', 'BRL', 'MXN', 'ZAR', 'NZD', 'SGD', 'HKD', 'SEK',
  'NOK', 'KRW', 'TRY', 'PLN',
];

/**
 * Hub-and-spoke: every hub × every ticker (forward) + every non-hub ticker ×
 * every hub (reverse). Pure — given the same ticker list, returns the same
 * pairs the route pre-renders.
 */
export function buildConvertParams(
  tickers: string[],
): { from: string; to: string }[] {
  const out: { from: string; to: string }[] = [];
  const hubSet = new Set(HUB_TICKERS);
  // Pass 1: every hub × every ticker (forward).
  for (const from of HUB_TICKERS) {
    for (const to of tickers) {
      if (from === to) continue;
      out.push({ from, to });
    }
  }
  // Pass 2: every non-hub ticker → every hub (reverse); hub×hub already in pass 1.
  for (const from of tickers) {
    if (hubSet.has(from)) continue;
    for (const to of HUB_TICKERS) {
      if (from === to) continue;
      out.push({ from, to });
    }
  }
  return out;
}
