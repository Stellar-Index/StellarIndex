import type { Metadata } from 'next';
import { LivePrice } from '../../LivePrice';

// The /v1/assets row shape, derived from the generated OpenAPI
// contract via the shared alias in src/api/hooks.ts (spec `Asset`
// plus the spec'd since board #33; narrowed-to-required here coin-overlay fields this widget reads:
// price_history_24h / change_1h_pct / change_7d_pct).
import type { Coin } from '@/api/hooks';
import { API_BASE_URL } from '@/api/client';

const isCIStub =
  API_BASE_URL.includes('.invalid') || API_BASE_URL.includes('local-stub');

const BUILD_FETCH_TIMEOUT_MS = 8_000;

type Params = Promise<{ slug: string }>;

interface AssetIndex {
  // Canonical slugs as returned by the listing — drives
  // generateStaticParams' route enumeration.
  slugs: string[];
  // Lowercased slug / asset_id → canonical asset_id. Used by the chart
  // sparkline fallback: /v1/chart rejects catalogue slugs (strict
  // canonical form only) and the thin GlobalAssetView these slugs return
  // carries no asset_id, so we resolve it from the listing here.
  byKey: Map<string, string>;
}

let assetIndexPromise: Promise<AssetIndex> | null = null;

// getAssetIndex fetches the /v1/assets listing ONCE per build and indexes
// it by slug + asset_id. This is a static-export server component, so the
// fetch runs at build time only — zero weight in the shipped embed
// bundle. Shared by generateStaticParams (slug enumeration) and the
// per-page chart-sparkline fallback (slug → canonical asset_id).
function getAssetIndex(): Promise<AssetIndex> {
  if (assetIndexPromise) return assetIndexPromise;
  assetIndexPromise = (async () => {
    const slugs: string[] = [];
    const byKey = new Map<string, string>();
    if (isCIStub) return { slugs, byKey };
    try {
      const res = await fetch(`${API_BASE_URL}/v1/assets?limit=500`, {
        signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS),
      });
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      const env = (await res.json()) as {
        data: { slug?: string; asset_id?: string }[];
      };
      for (const row of env.data ?? []) {
        const slug = row.slug || row.asset_id || '';
        if (slug) slugs.push(slug);
        const id = row.asset_id || '';
        if (id) {
          if (row.slug) byKey.set(row.slug.toLowerCase(), id);
          byKey.set(id.toLowerCase(), id);
        }
      }
    } catch {
      // Leave empty — generateStaticParams falls back to the single
      // anchor slug, and the sparkline fallback simply renders no chart.
    }
    return { slugs, byKey };
  })();
  return assetIndexPromise;
}

// resolveChartAsset picks the canonical asset id to query /v1/chart with.
// XLM/native and any AssetDetail response already carry coin.asset_id; a
// thin GlobalAssetView (catalogue slugs like usdc, aqua) has none, so we
// resolve it from the listing index by slug.
//
// Discriminated on `kind` (ADR-0042 LC-040) rather than `coin.asset_id`
// truthiness alone: the former shape-sniff would have (in theory) let a
// GlobalAssetView payload with a stray non-empty asset_id-shaped field
// short-circuit into the wrong branch. `kind === 'stellar_asset'` is the
// server's actual contract for "this response carries a real asset_id",
// so checking it first is the load-bearing condition; `coin.asset_id` is
// kept as a truthiness guard alongside it (belt-and-braces — an
// unexpected empty string on the stellar_asset branch still falls
// through to the index lookup instead of returning "").
function resolveChartAsset(
  slug: string,
  coin: Coin,
  index: AssetIndex,
): string | null {
  // Same edge-cache transition tolerance as fetchCoinDirect (2026-07-10,
  // remove after ~2026-07-17): a stale pre-v0.11 cache entry lacks
  // `kind`; asset_id present is unambiguously the stellar_asset shape.
  if (
    (coin.kind === undefined || (coin.kind as string) === 'stellar_asset') &&
    coin.asset_id
  ) {
    return coin.asset_id;
  }
  const norm = slug.toLowerCase();
  if (norm === 'xlm' || norm === 'native') return 'native';
  return index.byKey.get(norm) ?? null;
}

const chartSparklineMemo = new Map<
  string,
  Promise<{ t: string; p?: string | null }[]>
>();

// fetchChartSparkline pulls a trailing-24h hourly USD series from
// /v1/chart — the same endpoint the currency embed's fetchFxSeries uses —
// so catalogue slugs (whose thin GlobalAssetView carries no
// price_history_24h) still get a real sparkline. The widget's price is
// the asset's USD price, so the pairing is <asset> vs fiat:USD. Memoised
// per asset so the ~3 casing variants of one slug share a single
// build-time call (the /v1/assets rate-limit lesson, audit 2026-06-19).
// Degrades to [] on any error → the card just omits the sparkline.
function fetchChartSparkline(
  asset: string,
): Promise<{ t: string; p?: string | null }[]> {
  const cached = chartSparklineMemo.get(asset);
  if (cached) return cached;
  const p = (async () => {
    if (isCIStub) return [];
    try {
      const res = await fetch(
        `${API_BASE_URL}/v1/chart?asset=${encodeURIComponent(asset)}&quote=fiat:USD&timeframe=24h&granularity=1h`,
        { signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS) },
      );
      if (!res.ok) return [];
      const env = (await res.json()) as {
        data?: { points?: { t: string; p?: string | null }[] };
      };
      return (env.data?.points ?? []).map((pt) => ({
        t: pt.t,
        p: pt.p ?? null,
      }));
    } catch {
      return [];
    }
  })();
  chartSparklineMemo.set(asset, p);
  return p;
}

export async function generateStaticParams() {
  const fallback = [{ slug: 'XLM' }];
  if (isCIStub) return fallback;
  try {
    const { slugs } = await getAssetIndex();
    if (slugs.length === 0) return fallback;
    // Always include XLM + native explicitly, and emit BOTH cases for
    // every slug — embeds are hand-typed into 3rd-party iframe src=
    // attributes where lowercase is the natural instinct, and the audit
    // (2026-06-19) found /embed/asset/xlm etc. 404'd.
    const seen = new Set<string>();
    const out: { slug: string }[] = [];
    for (const slug of ['XLM', 'native', ...slugs]) {
      for (const variant of [slug, slug.toLowerCase(), slug.toUpperCase()]) {
        if (variant && !seen.has(variant)) {
          seen.add(variant);
          out.push({ slug: variant });
        }
      }
    }
    return out;
  } catch {
    return fallback;
  }
}

export async function generateMetadata({
  params,
}: {
  params: Params;
}): Promise<Metadata> {
  const { slug } = await params;
  return {
    title: `${slug} — embeddable price widget`,
    description: `Iframe-friendly Stellar price ticker for ${slug}. Designed to be dropped into a customer site at any width.`,
    robots: { index: false, follow: false },
  };
}

async function fetchCoin(slug: string): Promise<Coin | null> {
  // Per-asset detail fetched from /v1/assets/{slug}, the
  // CoinSummary-superset surface (rc.46 R-018 final). The `Coin`
  // type alias here is kept for stability; field shape is identical
  // for the read columns this embed renders (price_usd /
  // change_*_pct / volume_24h_usd / price_history_24h / code).
  if (isCIStub) return null;
  // XLM / native: /v1/assets/XLM returns the THIN GlobalAssetView
  // (price only — no change chips, sparkline, or 24h volume) and can
  // collide with the wrapped-XLM classic asset; native returns the full
  // AssetDetail. Resolve XLM→native so the flagship widget isn't
  // degraded (audit 2026-06-19).
  const norm = slug.toLowerCase();
  const id = norm === 'xlm' || norm === 'native' ? 'native' : slug;
  try {
    const res = await fetch(
      `${API_BASE_URL}/v1/assets/${encodeURIComponent(id)}`,
      { signal: AbortSignal.timeout(BUILD_FETCH_TIMEOUT_MS) },
    );
    if (!res.ok) return null;
    const env = (await res.json()) as { data: Coin };
    return env.data ?? null;
  } catch {
    return null;
  }
}

/**
 * /embed/asset/[slug] — minimal price widget designed to be iframed.
 *
 * No navbar, no footer, no global font weight overrides. Just the
 * price + 24h change + sparkline + "powered by" attribution. The
 * route uses a custom layout (siblings of layout.tsx via app router)
 * so the global Navbar / Footer don't render.
 *
 * Recommended iframe shape:
 *   <iframe src="https://stellarindex.io/embed/asset/USDC"
 *           width="320" height="160" frameborder="0"
 *           sandbox="allow-scripts"></iframe>
 *
 * SEO is opted out (robots: noindex) — these are widgets, not
 * destination pages.
 */
export default async function EmbedAssetPage({ params }: { params: Params }) {
  const { slug } = await params;
  const [coin, index] = await Promise.all([fetchCoin(slug), getAssetIndex()]);

  if (!coin) {
    return (
      <div className="flex h-full min-h-32 items-center justify-center px-3 py-3 text-sm text-ink-muted">
        <span>No data for {slug}</span>
      </div>
    );
  }

  const priceNum = coin.price_usd ? Number(coin.price_usd) : null;
  const change1h = coin.change_1h_pct ? Number(coin.change_1h_pct) : null;
  const change24h = coin.change_24h_pct ? Number(coin.change_24h_pct) : null;
  const change7d = coin.change_7d_pct ? Number(coin.change_7d_pct) : null;
  // The canonical asset id for /v1/price + /v1/chart. Present on
  // AssetDetail (native + Stellar asset_ids); resolved from the listing
  // for thin GlobalAssetView catalogue slugs (usdc, aqua, …).
  const chartAsset = resolveChartAsset(slug, coin, index);
  // Sparkline points: prefer the inlined 24h history (AssetDetail); fall
  // back to a /v1/chart series so catalogue slugs get a real sparkline
  // instead of none.
  let points = coin.price_history_24h ?? [];
  if (points.length === 0 && chartAsset) {
    points = await fetchChartSparkline(chartAsset);
  }
  // Thin GlobalAssetView carries `ticker`, not `code`; fall back so the
  // header renders a label rather than a blank.
  const code = coin.code ?? (coin as { ticker?: string }).ticker ?? slug;
  // LivePrice needs a canonical asset id; the resolved chart asset covers
  // catalogue slugs so their price refreshes live too.
  const liveAssetId = coin.asset_id ?? chartAsset ?? '';

  return (
    <div className="flex h-full min-h-32 flex-col gap-2 bg-surface px-4 py-3 text-ink">
      <div className="flex items-baseline justify-between gap-2">
        <div className="flex items-baseline gap-2">
          <span className="text-base font-semibold tracking-tight">
            {code}
          </span>
          <span className="font-mono text-[10px] text-ink-muted">
            Stellar
          </span>
        </div>
        <a
          href={`https://stellarindex.io/assets/${slug}`}
          target="_blank"
          rel="noreferrer noopener"
          className="text-[10px] text-ink-faint hover:text-brand-600"
        >
          stellarindex.io ↗
        </a>
      </div>
      <div className="flex flex-wrap items-baseline gap-x-2 gap-y-1">
        <LivePrice
          assetId={liveAssetId}
          initial={priceNum != null ? formatPrice(priceNum) : '—'}
        />
        <ChangeChip pct={change1h} label="1h" />
        <ChangeChip pct={change24h} label="24h" />
        <ChangeChip pct={change7d} label="7d" />
      </div>
      {points.length > 0 && <Sparkline points={points} />}
      <div className="mt-auto flex items-center justify-between text-[10px] text-ink-faint">
        <span>Powered by Stellar Index</span>
        {coin.volume_24h_usd && (
          <span className="font-mono tabular-nums">
            ${formatCompact(Number(coin.volume_24h_usd))} 24h vol
          </span>
        )}
      </div>
    </div>
  );
}

function ChangeChip({ pct, label }: { pct: number | null | undefined; label: string }) {
  if (pct == null || !Number.isFinite(pct)) return null;
  const cls =
    pct > 0
      ? 'bg-up-subtle text-up'
      : pct < 0
        ? 'bg-down-subtle text-down'
        : 'bg-surface-subtle text-ink-body';
  return (
    <span className={`rounded-sm px-1.5 py-0.5 font-mono text-[11px] tabular-nums ${cls}`}>
      {pct > 0 ? '+' : ''}
      {pct.toFixed(2)}% {label}
    </span>
  );
}

function Sparkline({ points }: { points: { p?: string | null }[] }) {
  const w = 280;
  const h = 32;
  // Resolve indexed (x, y) honouring nulls so contiguous segments
  // can close back to the baseline at gap boundaries (the area
  // shouldn't bridge across hours with no trade).
  const valid = points
    .map((pt, i) => ({ i, n: pt.p ? Number(pt.p) : null }))
    .filter((p) => p.n != null && Number.isFinite(p.n) && p.n > 0);
  if (valid.length < 2) return null;
  const min = Math.min(...valid.map((p) => p.n!));
  const max = Math.max(...valid.map((p) => p.n!));
  const range = max - min || max * 0.01;
  const xStep = points.length > 1 ? w / (points.length - 1) : 0;
  const segs: string[] = [];
  let pen = false;
  type Run = { start: number; end: number };
  const runs: Run[] = [];
  let runStart = -1;
  let lastIdx = -1;
  points.forEach((pt, i) => {
    const n = pt.p ? Number(pt.p) : null;
    if (n == null || !Number.isFinite(n) || n <= 0) {
      if (pen && runStart >= 0) runs.push({ start: runStart, end: lastIdx });
      pen = false;
      runStart = -1;
      return;
    }
    const x = i * xStep;
    const y = h - ((n - min) / range) * h;
    segs.push(`${pen ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`);
    if (!pen) runStart = i;
    pen = true;
    lastIdx = i;
  });
  if (pen && runStart >= 0) runs.push({ start: runStart, end: lastIdx });
  const trendUp = valid[valid.length - 1].n! >= valid[0].n!;
  const fill = trendUp ? 'rgba(16,185,129,0.14)' : 'rgba(244,63,94,0.14)';
  // Area path: one closed sub-region per contiguous run.
  const areaSegs: string[] = [];
  for (const run of runs) {
    let started = false;
    for (let i = run.start; i <= run.end; i++) {
      const n = points[i]?.p ? Number(points[i].p) : null;
      if (n == null || !Number.isFinite(n) || n <= 0) continue;
      const x = i * xStep;
      const y = h - ((n - min) / range) * h;
      areaSegs.push(`${started ? 'L' : 'M'} ${x.toFixed(1)} ${y.toFixed(1)}`);
      started = true;
    }
    const xStart = run.start * xStep;
    const xEnd = run.end * xStep;
    areaSegs.push(`L ${xEnd.toFixed(1)} ${h} L ${xStart.toFixed(1)} ${h} Z`);
  }
  return (
    <svg
      viewBox={`0 0 ${w} ${h}`}
      preserveAspectRatio="none"
      className="h-8 w-full"
      aria-label="24-hour price sparkline"
    >
      <path d={areaSegs.join(' ')} stroke="none" fill={fill} />
      <path
        d={segs.join(' ')}
        fill="none"
        strokeWidth="1.5"
        className={trendUp ? 'stroke-emerald-500' : 'stroke-rose-500'}
      />
    </svg>
  );
}

function formatPrice(n: number): string {
  if (!Number.isFinite(n)) return '—';
  if (n >= 1) return `$${n.toFixed(n >= 100 ? 2 : 4)}`;
  if (n >= 0.001) return `$${n.toFixed(6)}`;
  if (n > 0) return `$${n.toExponential(3)}`;
  return '—';
}

function formatCompact(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '—';
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(0)}k`;
  return n.toFixed(2);
}
