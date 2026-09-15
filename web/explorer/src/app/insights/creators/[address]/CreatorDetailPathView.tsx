'use client';

import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { AccountRelationView } from '../../AccountRelationView';

// Reads the real account id (G-strkey) from the path at runtime. There
// are 955,023 creators; no build enumerates them, so the CF Function
// serves the one built shell for every /insights/creators/{g} and this
// reads the segment back out.
//
// G-strkeys are case-sensitive base32 — never lowercased.
export function CreatorDetailPathView() {
  return (
    <AccountRelationView account={useLastPathSegment()} relation="created" />
  );
}
