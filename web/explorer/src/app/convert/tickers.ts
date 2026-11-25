import { buildFetchData, isCIStub } from '@/lib/buildFetch';
import {
  buildAssetConvertParams,
  buildConvertParams,
  convertAssets,
  type ConvertAsset,
} from '@/lib/convert-params';

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
  slug?: string;
  class: string;
  asset_id?: string;
  issuer?: string;
}

export interface ConvertCatalogue {
  /** The fiat tickers the /convert routes build pages for. */
  fiat: string[];
  /** The verified Stellar credit assets, keyed by catalogue (code, issuer). */
  assets: ConvertAsset[];
}

let cataloguePromise: Promise<ConvertCatalogue> | null = null;

/** One /v1/assets/verified read per build, shared by the landing and every pair page. */
export function fetchConvertCatalogue(): Promise<ConvertCatalogue> {
  cataloguePromise ??= loadCatalogue();
  return cataloguePromise;
}

async function loadCatalogue(): Promise<ConvertCatalogue> {
  if (isCIStub) return { fiat: FALLBACK_TICKERS, assets: [] };
  // Routed through buildFetchData (not a raw fetch) so a transient
  // 429/5xx during static export is retried and, if it persists,
  // FAILS THE BUILD instead of silently shrinking this list — the
  // same fail-hard contract sitemap.ts uses for the identical
  // /v1/assets/verified listing. FALLBACK_TICKERS covers only a
  // genuinely empty listing, not a transport failure.
  const rows =
    (await buildFetchData<VerifiedCurrencyEntry[]>('/v1/assets/verified')) ??
    [];
  const tickers = rows
    .filter((row) => row.class === 'fiat')
    .map((row) => row.ticker)
    .filter(Boolean);
  const fiat = tickers.length > 0 ? tickers : FALLBACK_TICKERS;
  return { fiat, assets: convertAssets(rows, fiat) };
}

let universePromise: Promise<Map<string, string[]>> | null = null;

/**
 * The ids the pre-rendered /convert pages price against `quote`
 * (`fiat:<TO>`), derived from the same param builders as
 * generateStaticParams, so one memoised batch per quote serves every page.
 */
export async function convertPriceUniverse(quote: string): Promise<string[]> {
  universePromise ??= fetchConvertCatalogue().then(({ fiat, assets }) => {
    const byQuote = new Map<string, string[]>();
    const add = (id: string, to: string) => {
      const q = `fiat:${to.toUpperCase()}`;
      const ids = byQuote.get(q) ?? [];
      ids.push(id);
      byQuote.set(q, ids);
    };
    for (const p of buildConvertParams(fiat)) {
      add(`fiat:${p.from.toUpperCase()}`, p.to);
    }
    const idOf = new Map(
      assets.map((a) => [a.ticker.toUpperCase(), a.assetId]),
    );
    for (const p of buildAssetConvertParams(assets, fiat)) {
      const id = idOf.get(p.from);
      if (id) add(id, p.to);
    }
    return byQuote;
  });
  return (await universePromise).get(quote) ?? [];
}
