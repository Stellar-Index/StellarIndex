import type { Metadata } from 'next';

import { ExchangesView } from './ExchangesView';

export const metadata: Metadata = {
  title: 'Exchanges — connected CEXes',
  description:
    'Centralised exchanges feeding Stellar Index — Binance, Coinbase, Kraken, Bitstamp. 24h USD volume, trade count, pair coverage. Source: /v1/sources?include=stats.',
};

export default function ExchangesPage() {
  return <ExchangesView />;
}
