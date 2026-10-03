'use client';

import { OrderBookPanel } from '../../markets/[pair]/OrderBookPanel';

const USDC_ASSET_ID =
  'USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN';

/**
 * orderBookQuote — the SDEX counter an asset page shows depth against:
 * XLM, the book most classic assets are quoted in, or USDC for XLM itself.
 */
export function orderBookQuote(assetID: string): string {
  return assetID === 'native' ? USDC_ASSET_ID : 'native';
}

/**
 * AssetOrderBookPanel — live SDEX bid/ask depth for the asset on its
 * Markets tab. Renders nothing for non-classic ids (no SDEX book).
 */
export function AssetOrderBookPanel({ assetID }: { assetID: string }) {
  return <OrderBookPanel base={assetID} quote={orderBookQuote(assetID)} />;
}
