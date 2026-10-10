import Link from 'next/link';

import { assetHref } from '@/lib/fiat-slugs';

import type { VerifiedItem } from './verified-currencies';

/** One chip per verified Stellar currency; fiat lives on /external/assets. */
export function VerifiedStrip({ items }: { items: VerifiedItem[] }) {
  const onChain = items.filter((v) => v.class !== 'fiat');
  if (onChain.length === 0) return null;
  return (
    <nav aria-label="Verified currencies" className="flex flex-wrap gap-2">
      {onChain.map((v) => (
        <Link
          key={v.slug}
          href={assetHref(v.slug)}
          title={
            v.verified_issuer
              ? `${v.name} — verified by ${v.verified_issuer}`
              : v.name
          }
          className="border-line text-ink hover:bg-surface-muted inline-flex items-center gap-1 rounded-full border px-2.5 py-0.5 text-xs font-medium"
        >
          <span aria-hidden className="text-up">
            ✓
          </span>
          {v.ticker}
        </Link>
      ))}
    </nav>
  );
}
