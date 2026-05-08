import type { Metadata } from 'next';
import { Suspense } from 'react';

import { CurrenciesView } from './CurrenciesView';

export const metadata: Metadata = {
  title: 'Currencies — fiat forex coverage',
  description:
    'World fiat currencies — live USD-base rates from Massive (Polygon.io). Ticker, name, USD-denominated rate, inverse rate. Source: /v1/currencies.',
};

export default function CurrenciesPage() {
  // Suspense wrapper required because CurrenciesView reads
  // useSearchParams() (PR #1062 added URL state for filter+sort).
  // Next.js 15 requires Suspense around any client component that
  // reads search params during static export.
  return (
    <Suspense fallback={<div className="mx-auto max-w-7xl px-6 py-8 text-sm text-slate-500">Loading…</div>}>
      <CurrenciesView />
    </Suspense>
  );
}
