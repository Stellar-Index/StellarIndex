import type { Metadata } from 'next';

import { Breadcrumbs } from '@/components/ui';
import { SITE_OG_IMAGES } from '@/lib/seo';
import { CURRENT_NETWORK } from '@/lib/networks';
import { fetchTickers } from './tickers';
import { ConvertLanding } from './ConvertLanding';

export const metadata: Metadata = {
  title: 'Currency converter — XLM and fiat at live rates',
  description:
    'Convert XLM and the major fiat currencies at the live mid-market rate, with the rate history charted underneath.',
  alternates: { canonical: `${CURRENT_NETWORK.explorerUrl}/convert` },
  openGraph: {
    title: 'Currency converter',
    description: 'Live XLM and fiat conversion rates, charted.',
    url: `${CURRENT_NETWORK.explorerUrl}/convert`,
    type: 'website',
    images: SITE_OG_IMAGES,
  },
};

export default async function ConvertIndexPage() {
  const tickers = await fetchTickers();
  return (
    <div className="mx-auto max-w-4xl space-y-6 px-6 py-8">
      <Breadcrumbs
        items={[{ label: 'Home', href: '/' }, { label: 'Convert' }]}
      />
      <ConvertLanding tickers={tickers} />
    </div>
  );
}
