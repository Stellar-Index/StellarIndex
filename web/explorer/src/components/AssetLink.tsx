'use client';

import Link from 'next/link';

import { useSACWrappers } from '@/api/hooks';
import { cn } from '@/lib/cn';
import { AssetLabel } from './AssetLabel';

/**
 * normalizeColonForm rewrites a classic-asset id served in the colon
 * form (`<CODE>:<G-issuer>`) to the dash form (`<CODE>-<G-issuer>`)
 * every other asset-linking path on the site expects.
 *
 * The colon form shows up on GET /v1/accounts/{g}/movements' post-P23
 * tail (ADR-0048 D5): the handler resolves a SAC's asset name from its
 * on-chain CAP-67 METADATA, which Stellar itself encodes as
 * "CODE:GISSUER" (internal/api/v1/explorer/movements.go,
 * resolveSEP41MovementAsset / SACAssetFromEvents) — verified against
 * the live API (e.g. "USDC:GA5ZSEJY…", "SILICA:GBDJWO2Q…"). AssetLabel
 * already normalises this for display (site-audit S-014); this mirrors
 * it here so the slug/link agrees with what the label renders instead
 * of falling back to a dead (unlinked) asset. Deliberately narrow: only
 * a short all-alnum code followed by `:G<55 base32>` qualifies, so
 * `pool:<hex>` liquidity-pool ids and raw Soroban contract ids (which
 * never contain a colon) are left untouched.
 */
function normalizeColonForm(canonical: string): string {
  return /^[A-Za-z0-9]{1,12}:G[A-Z2-7]{55}$/.test(canonical)
    ? canonical.replace(':', '-')
    : canonical;
}

/**
 * assetSlug maps a canonical asset_id to the SHORT slug that
 * /assets/[slug] actually pre-renders under static export.
 *
 * CRITICAL: long-form ids (`USDC-GA5Z…`, raw SAC `C…`) are NOT in
 * generateStaticParams (it indexes by short slug to keep the route
 * count sane), so linking to them hard-404s. We therefore link to the
 * short code/ticker form (`/assets/USDC`, `/assets/BTC`, `/assets/XLM`)
 * which is pre-rendered for every verified currency + the top ~500
 * assets — i.e. everything that shows up in these tables. Returns null
 * when there's no safe linkable slug (the caller renders a plain label).
 */
export function assetSlug(canonical: string | undefined | null): string | null {
  if (!canonical) return null;
  if (canonical === 'native' || /^\d+$/.test(canonical)) return 'native';
  if (canonical.startsWith('fiat:')) return canonical.slice(5) || null;
  if (canonical.startsWith('crypto:')) return canonical.slice(7) || null;
  // Raw SAC contract id — only linkable once resolved to a classic
  // asset (handled in AssetLink via the wrapper map); not here.
  if (/^C[A-Za-z0-9]{55}$/.test(canonical)) return null;
  canonical = normalizeColonForm(canonical);
  const dashIx = canonical.indexOf('-');
  if (dashIx === -1) {
    // Bare code (rare) — link only if it's a plausible asset code.
    return canonical.length <= 12 ? canonical : null;
  }
  return canonical.slice(0, dashIx) || null;
}

/**
 * AssetLink — AssetLabel wrapped in a link to the asset's detail page,
 * targeting the static-export-safe short slug. Falls back to a plain
 * (unlinked) AssetLabel when the id has no safe route (e.g. an
 * unresolved SAC contract). Use this anywhere a table/cell shows an
 * asset so every asset reference is a click-through.
 */
export function AssetLink({
  canonical,
  className,
}: {
  canonical: string | undefined | null;
  className?: string;
}) {
  const { data: sacMap } = useSACWrappers();

  let slug = assetSlug(canonical);
  // Resolve a SAC contract to its wrapped classic asset's code so the
  // link still works (the raw C-address has no asset page).
  if (!slug && canonical && /^C[A-Za-z0-9]{55}$/.test(canonical)) {
    const resolved = sacMap?.[canonical];
    if (resolved === 'native') slug = 'native';
    else if (resolved) {
      const i = resolved.indexOf('-');
      slug = i === -1 ? resolved : resolved.slice(0, i);
    }
  }

  if (!slug) return <AssetLabel canonical={canonical} />;
  return (
    <Link
      href={`/assets/${encodeURIComponent(slug)}`}
      className={cn('transition-colors hover:text-brand-600', className)}
    >
      <AssetLabel canonical={canonical} />
    </Link>
  );
}

/**
 * shortAssetText — compact single-line label for a canonical asset_id,
 * for dense table cells where AssetLabel's two-line form is too tall.
 */
export function shortAssetText(canonical: string | undefined | null): string {
  if (!canonical) return '—';
  if (canonical === 'native' || /^\d+$/.test(canonical)) return 'XLM';
  if (canonical.startsWith('fiat:')) return canonical.slice(5);
  if (canonical.startsWith('crypto:')) return canonical.slice(7);
  if (/^C[A-Za-z0-9]{55}$/.test(canonical)) return `${canonical.slice(0, 4)}…${canonical.slice(-4)}`;
  canonical = normalizeColonForm(canonical);
  const i = canonical.indexOf('-');
  if (i === -1) return canonical.length > 12 ? `${canonical.slice(0, 4)}…${canonical.slice(-4)}` : canonical;
  return canonical.slice(0, i);
}

/**
 * AssetText — compact single-line asset code linked to its asset page
 * (safe short slug). For dense analytics feeds (anomalies / divergence
 * / MEV) where the full AssetLabel would bloat the row. Renders plain
 * text when there's no safe route.
 */
export function AssetText({
  canonical,
  className,
}: {
  canonical: string | undefined | null;
  className?: string;
}) {
  const slug = assetSlug(canonical);
  const text = shortAssetText(canonical);
  if (!slug) return <span className={className}>{text}</span>;
  return (
    <Link
      href={`/assets/${encodeURIComponent(slug)}`}
      className={cn('transition-colors hover:text-brand-600 hover:underline', className)}
    >
      {text}
    </Link>
  );
}

/**
 * PairLink — links a (base, quote) pair to its market detail page.
 * /markets/[pair] pre-renders the long-form base~quote ids, so the
 * full canonical pair is the correct (and safe) link target here.
 * Renders the two AssetLabels with a separator unless given children.
 */
export function PairLink({
  base,
  quote,
  className,
  children,
}: {
  base: string;
  quote: string;
  className?: string;
  children?: React.ReactNode;
}) {
  const slug = `${base}~${quote}`;
  return (
    <Link
      href={`/markets/${encodeURIComponent(slug)}`}
      className={cn('inline-flex items-center gap-1 transition-colors hover:text-brand-600', className)}
    >
      {children ?? (
        <>
          <AssetLabel canonical={base} />
          <span className="text-ink-faint">/</span>
          <AssetLabel canonical={quote} />
        </>
      )}
    </Link>
  );
}
