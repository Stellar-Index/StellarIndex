import type { Metadata } from 'next';
import { Suspense } from 'react';

import { shellMetadata } from '@/lib/seo';

import { PoolPathView } from './PoolPathView';

// Shell-only: functions/liquidity-pools/[[path]].js serves it for any pool id.
export const dynamicParams = false;

export function generateStaticParams() {
  return [{ id: 'shell' }];
}

export const metadata: Metadata = shellMetadata(
  'Liquidity pool',
  'Reserves, price and constant-product depth for one Stellar native liquidity pool.',
);

export default function PoolDetailPage() {
  return (
    <Suspense fallback={null}>
      <PoolPathView />
    </Suspense>
  );
}
