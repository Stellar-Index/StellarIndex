// The closed set of /exchanges/<name> pages: generateStaticParams and
// sitemap.ts both read it, so the sitemap lists exactly the built pages.
export const CEX_INFO: Record<
  string,
  {
    name: string;
    type: string;
    homepage: string;
    docsUrl: string;
    blurb: string;
  }
> = {
  binance: {
    name: 'Binance',
    type: 'CEX — REST + WebSocket spot tickers',
    homepage: 'https://www.binance.com',
    docsUrl: 'https://github.com/binance/binance-spot-api-docs',
    blurb:
      'Spot trading pairs against XLM. We poll Binance ticker streams for trade events; usd_volume is computed Phase-1-style from USD-pegged quotes (USDT, BUSD, USDC).',
  },
  coinbase: {
    name: 'Coinbase',
    type: 'CEX — Advanced Trade WebSocket',
    homepage: 'https://www.coinbase.com',
    docsUrl: 'https://docs.cloud.coinbase.com/advanced-trade-api',
    blurb:
      'XLM spot pairs from Coinbase Advanced Trade — direct USD quote for usd_volume populates with no FX leg. The market-data feed dropped 0-quote-amount canonical-validator violations after the fix in PR #49.',
  },
  kraken: {
    name: 'Kraken',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.kraken.com',
    docsUrl: 'https://docs.kraken.com/websockets',
    blurb:
      'Kraken spot pairs against USD and EUR. Forex factor (X2.5) snaps EUR pairs into USD-equivalent volume.',
  },
  bitstamp: {
    name: 'Bitstamp',
    type: 'CEX — public WebSocket trades',
    homepage: 'https://www.bitstamp.net',
    docsUrl: 'https://www.bitstamp.net/websocket/v2/',
    blurb:
      'Long-running USD-quoted XLM pairs. Smaller volume share than Binance/Coinbase but contributes to the cross-CEX VWAP weighting.',
  },
};
