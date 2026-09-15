'use client';

import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { AccountRelationView } from '../../AccountRelationView';

// Reads the real account id (G-strkey) from the path at runtime. There
// are 2,427 sponsors and no way to know which one a URL names at build
// time, so the CF Function serves the one built shell for every
// /insights/sponsors/{g} and this reads the segment back out.
//
// G-strkeys are case-sensitive base32 — never lowercased.
export function SponsorDetailPathView() {
  return (
    <AccountRelationView account={useLastPathSegment()} relation="sponsored" />
  );
}
