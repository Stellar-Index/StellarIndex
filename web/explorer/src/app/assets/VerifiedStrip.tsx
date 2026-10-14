import Link from 'next/link';
import { BadgeCheck } from 'lucide-react';

import { assetHref } from '@/lib/fiat-slugs';

import type { VerifiedItem } from './verified-currencies';

/** One chip per verified Stellar currency; fiat lives on /external/assets. */
export function VerifiedStrip({ items }: { items: VerifiedItem[] }) {
  const onChain = items.filter((v) => v.class !== 'fiat');
  if (onChain.length === 0) return null;
  return (
    <nav
      aria-label="Verified currencies"
      className="flex [scrollbar-width:none] items-center gap-2 overflow-x-auto md:flex-wrap md:overflow-visible"
    >
      <span className="text-ink-muted mr-1 shrink-0 text-xs">Verified</span>
      {onChain.map((v) => (
        <Link
          key={v.slug}
          href={assetHref(v.slug)}
          title={
            v.verified_issuer
              ? `${v.name} — verified by ${v.verified_issuer}`
              : v.name
          }
          className="ring-line text-ink hover:bg-surface-muted hover:ring-line-strong inline-flex h-6 shrink-0 items-center gap-1.5 rounded-full px-2.5 text-xs font-medium ring-1 transition-colors duration-150 ring-inset"
        >
          <BadgeCheck className="text-up h-3 w-3" aria-hidden="true" />
          {v.ticker}
        </Link>
      ))}
    </nav>
  );
}
