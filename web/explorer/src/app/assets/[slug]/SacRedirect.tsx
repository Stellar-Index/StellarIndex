'use client';

import { useEffect } from 'react';
import { useRouter } from 'next/navigation';

import { Skeleton } from '@/components/ui';

const CONTRACT = /^C[A-Z2-7]{55}$/;

/**
 * The page to send a SAC contract URL to: /v1/assets resolves a classic asset's SAC to that
 * asset, so /assets/{SAC} would be a second page for it. Null when the slug is not a contract or
 * already is the asset's own slug (a Soroban-native token).
 */
export function sacRedirectHref(
  slug: string,
  resolvedSlug: string | null | undefined,
): string | null {
  if (!CONTRACT.test(slug) || !resolvedSlug || resolvedSlug === slug) {
    return null;
  }
  return `/assets/${encodeURIComponent(resolvedSlug)}/`;
}

/** Replaces the URL with the asset's own page; renders a placeholder meanwhile. */
export function SacRedirect({ href }: { href: string }) {
  const router = useRouter();
  useEffect(() => {
    router.replace(href);
  }, [href, router]);
  return (
    <div className="space-y-4" aria-busy="true">
      <Skeleton className="h-8 w-64" />
      <Skeleton className="h-40 w-full" />
    </div>
  );
}
