'use client';

import { useEffect, type ReactNode } from 'react';
import { useRouter, useSearchParams } from 'next/navigation';

/**
 * Canonicalises the legacy query-param entity URLs onto the new plural path
 * routes (SEO plan D-redirect set). `_redirects` can't match query strings, so
 * we do it client-side: if the legacy `?{param}=` is present, replace() to
 * `{base}/{value}/`; otherwise render the page's normal empty-state view.
 *
 *   /tx?hash=H        -> /transactions/H/
 *   /ledger?seq=N     -> /ledgers/N/
 *   /contract?id=C    -> /contracts/C/
 *   /accounts?id=G    -> /accounts/G/
 *
 * Internal links already point at the new URLs (the link sweep), so this only
 * catches external bookmarks / shared old links.
 */
export function LegacyEntityRedirect({
  param,
  base,
  children,
}: {
  param: string;
  base: string;
  children: ReactNode;
}) {
  const sp = useSearchParams();
  const router = useRouter();
  const val = sp.get(param);
  useEffect(() => {
    if (val) router.replace(`${base}/${encodeURIComponent(val)}/`);
  }, [val, base, router]);
  if (val) return null; // redirecting — don't flash the old view
  return <>{children}</>;
}
