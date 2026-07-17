'use client';

import { useState } from 'react';

// SidebarAssetIcon — the asset-detail sidebar avatar. Renders the SEP-1
// icon when it's a well-formed https URL and the image actually loads;
// a missing/non-https/blocked URL (onError) falls back to the letter
// glyph. Mirrors HomeTopAssets.AssetIcon — a tiny client island so the
// surrounding sidebar can stay a server component (onError needs a
// client boundary). Plain <img> (not next/image): remote SEP-1 hosts
// can't be enumerated into a next/image domain allowlist under static
// export.
export function SidebarAssetIcon({
  image,
  code,
}: {
  image?: string | null;
  code: string;
}) {
  const [broken, setBroken] = useState(false);
  const safe = typeof image === 'string' && /^https:\/\//i.test(image);
  if (safe && !broken) {
    return (
      // eslint-disable-next-line @next/next/no-img-element -- remote SEP-1 icons; next/image needs a domain allowlist we can't enumerate under static export
      <img
        src={image!}
        alt=""
        width={36}
        height={36}
        loading="lazy"
        onError={() => setBroken(true)}
        className="h-9 w-9 rounded-full bg-surface-subtle object-contain"
      />
    );
  }
  return (
    <span
      aria-hidden
      className="flex h-9 w-9 items-center justify-center rounded-full bg-surface-subtle font-mono text-sm font-semibold text-ink"
    >
      {code.slice(0, 1)}
    </span>
  );
}
