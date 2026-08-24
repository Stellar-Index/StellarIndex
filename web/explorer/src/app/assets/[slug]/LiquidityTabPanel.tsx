import Link from 'next/link';

import { Panel } from '@/components/reveal';
import { asExample, API_BASE_URL } from '@/api/client';
import { formatCompact, formatPairPrice } from '@/lib/format';
// /v1/pools row from the generated OpenAPI contract (spec PoolRow, via
// the shared alias in src/api/hooks.ts).
import type { Pool as PoolRow } from '@/api/hooks';
import { isCIStub } from '@/lib/buildFetch';
import { shortAssetText } from '@/lib/asset-label';

const BUILD_FETCH_TIMEOUT_MS = 8_000;

/**
 * fetchPoolsForAsset returns the pool rows, or `null` when we could not
 * read them at all (CI stub, non-2xx, timeout, transport error).
 *
 * The null is load-bearing: /v1/pools answers 503 on its documented
 * trades-hypertable ceiling, and this panel's empty state asserts "no
 * DEX pools observed touching {code} in the trailing 14 days" — a claim
 * that would otherwise be BAKED INTO THE STATIC EXPORT off a transport
 * blip (the incident class src/lib/buildFetch.ts's header documents).
 */
async function fetchPoolsForAsset(assetID: string): Promise<PoolRow[] | null> {
  if (isCIStub) return null;
  try {
    const url = `${API_BASE_URL}/v1/pools?asset=${encodeURIComponent(assetID)}&limit=100&order_by=volume_24h_usd_desc`;
    const res = await fetch(url, {
      signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
    });
    if (!res.ok) return null;
    const env = (await res.json()) as { data?: PoolRow[] };
    return env.data ?? [];
  } catch {
    return null;
  }
}

/**
 * LiquidityTabPanel — every DEX pool that touches this asset on
 * either side, ranked by 24h USD volume. Single fetch to
 * `/v1/pools?asset=<assetID>` (the OR-shape filter) — pre-2026-05-09
 * this was two parallel `?base=` + `?quote=` fetches merged
 * client-side, but the API now does the OR predicate server-side
 * so we get one cache key, one SQL scan, and a smaller cached
 * payload.
 *
 * Server component; fetched at request time. Empty-state when the
 * asset has no DEX activity in the recency window.
 */
export async function LiquidityTabPanel({
  assetID,
  code,
}: {
  assetID: string;
  code: string;
}) {
  const rows = await fetchPoolsForAsset(assetID);
  // Defensive client-side filter: older API versions silently
  // ignore unknown query params, so on a pre-`?asset=` deployment
  // the response would be the global top pools (every asset
  // detail page would render the same list). Drop this once every
  // region runs a release that includes the filter.
  const merged = (rows ?? [])
    .filter((p) => p.base === assetID || p.quote === assetID)
    .map((p) => ({
      ...p,
      side: (p.base === assetID ? 'base' : 'quote') as 'base' | 'quote',
    }))
    .sort((a, b) => {
      const av = Number(a.volume_24h_usd ?? '0');
      const bv = Number(b.volume_24h_usd ?? '0');
      return (Number.isFinite(bv) ? bv : 0) - (Number.isFinite(av) ? av : 0);
    });

  return (
    <Panel
      title={`Liquidity — every DEX pool that touches ${code}`}
      hint="Per-source breakdown across DEXes. Backed by /v1/pools?asset= (base OR quote)."
      source={asExample('/v1/pools', {
        asset: assetID,
        limit: 100,
        order_by: 'volume_24h_usd_desc',
      })}
      bodyClassName="-mx-4"
    >
      {rows == null ? (
        <p className="text-ink-muted px-4 py-3 text-sm">
          Pool list unavailable for this build — the liquidity query didn&apos;t
          answer, so {code}&apos;s DEX pools are unknown rather than absent. It
          refreshes on the next build.
        </p>
      ) : merged.length === 0 ? (
        <p className="text-ink-muted px-4 py-3 text-sm">
          No DEX pools observed touching {code} in the trailing 14 days. Either
          the asset only trades on CEX feeds or the dispatcher hasn&apos;t
          decoded a swap involving it yet.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <table className="divide-line min-w-full divide-y text-sm">
            <thead>
              <tr className="text-ink-muted text-left text-[10px] tracking-wider uppercase">
                <th className="px-4 py-2 font-medium">Venue</th>
                <th className="px-4 py-2 font-medium">Pair</th>
                <th className="px-4 py-2 font-medium">Side</th>
                <th className="px-4 py-2 text-right font-medium">Last price</th>
                <th className="px-4 py-2 text-right font-medium">24h volume</th>
                <th className="px-4 py-2 text-right font-medium">24h trades</th>
              </tr>
            </thead>
            <tbody className="divide-line-subtle divide-y">
              {merged.map((p) => {
                const slug = encodeURIComponent(`${p.base}~${p.quote}`);
                const lp = p.last_price ? Number(p.last_price) : null;
                const lpFixed = lp == null ? null : formatPairPrice(lp);
                return (
                  <tr
                    key={`${p.source}|${p.base}|${p.quote}|${p.side}`}
                    className="hover:bg-surface-muted"
                  >
                    <td className="px-4 py-2">
                      <Link
                        href={`/sources/${p.source}`}
                        className="text-ink-body hover:text-brand-600 font-mono text-xs tracking-wider uppercase"
                      >
                        {p.source}
                      </Link>
                    </td>
                    <td className="px-4 py-2">
                      <Link
                        href={`/markets/${slug}`}
                        className="hover:text-brand-600 font-mono text-xs"
                      >
                        {shortAssetText(p.base)} / {shortAssetText(p.quote)}
                      </Link>
                    </td>
                    <td className="px-4 py-2">
                      <span className="bg-surface-subtle text-ink-body rounded-sm px-1.5 py-0.5 text-[10px] tracking-wider uppercase">
                        {p.side}
                      </span>
                    </td>
                    <td className="px-4 py-2 text-right">
                      {lpFixed ? (
                        <span className="text-ink-body font-mono tabular-nums">
                          {lpFixed}
                        </span>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-right">
                      {p.volume_24h_usd ? (
                        <span className="font-mono tabular-nums">
                          ${formatCompact(Number(p.volume_24h_usd))}
                        </span>
                      ) : (
                        <span className="text-ink-faint">—</span>
                      )}
                    </td>
                    <td className="px-4 py-2 text-right">
                      <span className="text-ink-muted font-mono tabular-nums">
                        {formatCompact(p.trade_count_24h)}
                      </span>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}
    </Panel>
  );
}
