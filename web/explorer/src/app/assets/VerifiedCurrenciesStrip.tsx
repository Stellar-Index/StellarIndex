import Link from 'next/link';

import { API_BASE_URL } from '@/api/client';

/**
 * Mirror of `VerifiedCurrencyListItem` on the wire.
 * See `internal/api/v1/assets_global.go`.
 */
export interface VerifiedItem {
  ticker: string;
  slug: string;
  name: string;
  verified_issuer?: string;
  network_count: number;
  networks: Array<{
    network: string;
    data_quality: 'indexed' | 'external';
  }>;
}

// CI builds use a stub hostname that doesn't resolve; bypass the
// network fetch in that case so static export doesn't time out.
const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

const BUILD_FETCH_TIMEOUT_MS = 8_000;

/**
 * fetchVerifiedCurrencies is the shared `/v1/assets/verified`
 * fetcher consumed by both this strip and the AssetsTable. Single
 * server-side fetch per page render — the page calls this once,
 * passes the result to both components as a prop.
 */
export async function fetchVerifiedCurrencies(): Promise<VerifiedItem[]> {
  if (isCIStub) return [];
  try {
    const res = await fetch(`${API_BASE_URL}/v1/assets/verified`, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
    });
    if (!res.ok) return [];
    const env = (await res.json()) as { data?: VerifiedItem[] };
    return env.data ?? [];
  } catch {
    return [];
  }
}

/**
 * VerifiedCurrenciesStrip renders a curated chip-row of every
 * verified currency in the catalogue at the top of `/assets`.
 *
 * Takes the pre-fetched verified list as a prop so the page-level
 * fetch can be shared with the AssetsTable (which uses the slug set
 * to mark rows with a verified badge). Empty array → returns null.
 *
 * Each chip links to `/assets/{slug}` — the global view added in
 * R-018 Phase 1.5.
 */
export function VerifiedCurrenciesStrip({
  verified,
}: {
  verified: VerifiedItem[];
}) {
  if (verified.length === 0) return null;

  return (
    <section className="space-y-3">
      <div className="flex items-baseline justify-between">
        <h2 className="text-sm font-medium uppercase tracking-wider text-slate-500 dark:text-slate-400">
          Verified currencies
        </h2>
        <span className="text-xs text-slate-500 dark:text-slate-400">
          {verified.length} cross-chain · catalogue
        </span>
      </div>
      <div className="flex flex-wrap gap-2">
        {verified.map((vc) => (
          <Link
            key={vc.slug}
            href={`/assets/${vc.slug}`}
            className="group inline-flex items-center gap-2 rounded-md border border-emerald-200 bg-white px-3 py-1.5 text-sm font-medium text-slate-800 transition hover:border-emerald-400 hover:bg-emerald-50 dark:border-emerald-900/40 dark:bg-slate-900 dark:text-slate-200 dark:hover:border-emerald-700 dark:hover:bg-emerald-950/30"
            title={
              vc.verified_issuer
                ? `${vc.name} — ${vc.verified_issuer}`
                : vc.name
            }
          >
            <svg
              xmlns="http://www.w3.org/2000/svg"
              viewBox="0 0 20 20"
              fill="currentColor"
              className="h-3.5 w-3.5 text-emerald-600 dark:text-emerald-400"
              aria-hidden="true"
            >
              <path
                fillRule="evenodd"
                d="M10 18a8 8 0 100-16 8 8 0 000 16zm3.707-9.293a1 1 0 00-1.414-1.414L9 10.586 7.707 9.293a1 1 0 00-1.414 1.414l2 2a1 1 0 001.414 0l4-4z"
                clipRule="evenodd"
              />
            </svg>
            <span>{vc.ticker}</span>
            <span className="text-[10px] text-slate-500 group-hover:text-slate-700 dark:text-slate-400 dark:group-hover:text-slate-300">
              {vc.network_count}
              <span className="ml-0.5">net{vc.network_count === 1 ? '' : 's'}</span>
            </span>
          </Link>
        ))}
      </div>
    </section>
  );
}
