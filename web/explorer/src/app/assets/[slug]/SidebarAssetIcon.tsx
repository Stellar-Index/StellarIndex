'use client';

import { useState } from 'react';

import { isSafePublicImageUrl } from '@/lib/safe-domain';

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
  // SEC-10: host-validated, not just scheme-validated — see
  // lib/safe-domain.ts (isSafePublicImageUrl) for the threat this closes.
  const safe = isSafePublicImageUrl(image) && !broken;
  if (safe) {
    return (
      // eslint-disable-next-line @next/next/no-img-element -- remote SEP-1 icons; next/image needs a domain allowlist we can't enumerate under static export
      <img
        src={image!}
        alt=""
        width={36}
        height={36}
        loading="lazy"
        onError={() => setBroken(true)}
        className="bg-surface-subtle h-9 w-9 rounded-full object-contain"
      />
    );
  }
  return (
    <span
      aria-hidden
      className="bg-surface-subtle text-ink flex h-9 w-9 items-center justify-center rounded-full font-mono text-sm font-semibold"
    >
      {code.slice(0, 1)}
    </span>
  );
}
