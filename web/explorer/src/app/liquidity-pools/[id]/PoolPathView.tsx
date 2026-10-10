'use client';

import { useLastPathSegment } from '@/lib/useLastPathSegment';

import { PoolView } from '../PoolView';

export function PoolPathView() {
  return <PoolView id={useLastPathSegment()} />;
}
