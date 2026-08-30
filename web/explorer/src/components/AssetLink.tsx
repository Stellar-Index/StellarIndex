'use client';

import Link from 'next/link';

import { useSACWrappers } from '@/api/hooks';
import { cn } from '@/lib/cn';
import { AssetLabel } from './AssetLabel';

import { isRawOracleAsset, normalizeColonForm, shortAssetText } from '@/lib/asset-label';

// Re-export for existing client-side importers; server code imports
// from '@/lib/asset-label' directly (see that module's header).
export { shortAssetText };

/**
 * assetSlug maps a canonical asset_id to the slug /assets/[slug] should
 * be linked under.
 *
 * It returns the FULL canonical id for a classic asset, not the bare
 * code. The bare code is ambiguous: every USDC-alike shares
 * `/assets/USDC`, so a link built from the code can resolve to a
 * DIFFERENT issuer's asset than the row the user clicked — including
 * resolving a scam issuer's token to the legitimate one's page, or the
 * reverse (wave-D EXR-02).
 *
 * This mirrors the decision already recorded for market pairs at
 * app/markets/[pair]/page.tsx (AM-09), and for the same reason:
 * generateStaticParams emits canonical `asset_id` routes for exactly
 * the same asset set as the short slugs (see the `cache.byAssetId` loop
 * there), so the canonical form never links worse and always links
 * precisely. Anything outside the pre-rendered set falls to the client
 * shell under BOTH spellings.
 *
 * The docstring here used to assert the opposite — that long-form ids
 * "hard-404" because generateStaticParams indexes only by short slug.
 * That stopped being true when asset_id routes were added (audit
 * 2026-06-19); the comment outlived the constraint.
 *
 * Returns null when there's no safe linkable slug (the caller renders a
 * plain label). Display labels are unaffected: callers keep using
 * shortAssetText, which is a separate, deliberately-short helper.
 */
export function assetSlug(canonical: string | undefined | null): string | null {
  if (!canonical) return null;
  if (canonical === 'native' || /^\d+$/.test(canonical)) return 'native';
  if (canonical.startsWith('fiat:')) return canonical.slice(5) || null;
  if (canonical.startsWith('crypto:')) return canonical.slice(7) || null;
  // Unmapped oracle symbol (`raw:<symbol>`) — no asset page exists for
  // it by definition; without this branch the classic split below would
  // link to /assets/raw%3A<symbol> (static-export 404).
  if (isRawOracleAsset(canonical)) return null;
  // Raw SAC contract id — only linkable once resolved to a classic
  // asset (handled in AssetLink via the wrapper map); not here.
  if (/^C[A-Za-z0-9]{55}$/.test(canonical)) return null;
  canonical = normalizeColonForm(canonical);
  const dashIx = canonical.indexOf('-');
  if (dashIx === -1) {
    // Bare code (rare) — link only if it's a plausible asset code.
    return canonical.length <= 12 ? canonical : null;
  }
  // The full `CODE-GISSUER…` id, not `canonical.slice(0, dashIx)`.
  return canonical;
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
    // The wrapped classic asset's FULL id, for the same reason as
    // assetSlug above — truncating here to the bare code re-introduces
    // the issuer ambiguity on the SAC path specifically.
    else if (resolved) slug = resolved;
  }

  if (!slug) return <AssetLabel canonical={canonical} />;
  return (
    <Link
      href={`/assets/${encodeURIComponent(slug)}`}
      className={cn('hover:text-brand-600 transition-colors', className)}
    >
      <AssetLabel canonical={canonical} />
    </Link>
  );
}

/**
 * AssetText — compact single-line asset code linked to its asset page.
 * For dense analytics feeds (anomalies / divergence / MEV) where the
 * full AssetLabel would bloat the row. Renders plain text when there's
 * no safe route.
 *
 * The LABEL stays short (shortAssetText) — these rows are dense by
 * design, and widening the visible text to a 56-char canonical id would
 * blow out every cell and chart legend that uses it. Only the href
 * carries the full id, with the canonical id in `title` so the issuer
 * is recoverable on hover without spending row width on it.
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
      title={canonical ?? undefined}
      className={cn(
        'hover:text-brand-600 transition-colors hover:underline',
        className,
      )}
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
      className={cn(
        'hover:text-brand-600 inline-flex items-center gap-1 transition-colors',
        className,
      )}
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
