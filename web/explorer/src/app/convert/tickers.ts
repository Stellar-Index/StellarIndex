import { buildFetchData, isCIStub } from '@/lib/buildFetch';

// Fallback majors so a brand-new build with no upstream still
// produces a meaningful matrix. Same set as /currencies/[ticker]'s
// fallback so the two routes stay aligned.
const FALLBACK_TICKERS = [
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
];

interface VerifiedCurrencyEntry {
  ticker: string;
  name?: string;
  class: string;
}

/** The fiat tickers the /convert routes build pages for. */
export async function fetchTickers(): Promise<string[]> {
  if (isCIStub) return FALLBACK_TICKERS;
  // Read from /v1/assets/verified; filter to class=fiat client-side.
  //
  // Routed through buildFetchData (not a raw fetch) so a transient
  // 429/5xx during static export is retried and, if it persists,
  // FAILS THE BUILD instead of silently shrinking this list — the
  // same fail-hard contract sitemap.ts's fetchCurrencyTickers uses
  // for the identical /v1/assets/verified listing. FALLBACK_TICKERS
  // below covers only a genuinely empty/never-populated listing (a
  // brand-new build with no upstream), not a transport failure.
  const rows = await buildFetchData<VerifiedCurrencyEntry[]>(
    '/v1/assets/verified',
  );
  const tickers = (rows ?? [])
    .filter((row) => row.class === 'fiat')
    .map((row) => row.ticker)
    .filter(Boolean);
  return tickers.length > 0 ? tickers : FALLBACK_TICKERS;
}
