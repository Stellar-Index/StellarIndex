import type { Metadata } from 'next';
import { Suspense } from 'react';
import { AssetsTable } from './AssetsTable';
import {
  VerifiedCurrenciesStrip,
  fetchVerifiedCurrencies,
} from './VerifiedCurrenciesStrip';

export const metadata: Metadata = {
  title: 'Assets — every token on Stellar',
  description:
    'Browse every classic and Soroban asset observed on Stellar — live price, 24h volume, market cap, supply, issuer. The canonical Stellar asset directory.',
};

/**
 * /assets — the explorer's asset directory.
 *
 * Server-component shell wraps a client-side table in Suspense so
 * the static export can pre-render the page chrome while the
 * client reads `?cursor=` / `?limit=` / `?issuer=` from the URL.
 */
export default async function AssetsPage() {
  // Single server-side fetch of the verified-currency catalogue
  // shared between the strip (renders each entry as a chip) and the
  // table (marks each row whose slug is in the catalogue with a
  // green check). One round-trip, no double fetch.
  const verified = await fetchVerifiedCurrencies();
  const verifiedSlugs = verified.map((v) => v.slug);

  return (
    <div className="mx-auto max-w-7xl space-y-6 px-6 py-8">
      <header className="space-y-2">
        <h1 className="text-2xl font-semibold tracking-tight text-ink">
          Assets
        </h1>
        <p className="max-w-3xl text-sm text-ink-body">
          Every classic + Soroban asset observed on Stellar. Live price
          via VWAP across on-chain DEXes, classic SDEX, and major
          off-chain venues. Click through for live charts, recent
          trades, supply detail, and issuer profile.
        </p>
      </header>
      <VerifiedCurrenciesStrip verified={verified} />
      <Suspense
        fallback={
          <div className="rounded-md border border-line bg-surface p-8 text-center text-sm text-ink-muted">
            Loading…
          </div>
        }
      >
        <AssetsTable verifiedSlugs={verifiedSlugs} />
      </Suspense>
    </div>
  );
}
