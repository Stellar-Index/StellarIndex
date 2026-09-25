import { apiGet } from '@/api/client';
import type { components } from '@/api/types';

type PriceBatchEnvelope = components['schemas']['PriceBatchEnvelope'];
export type PriceBatchRow = NonNullable<PriceBatchEnvelope['data']>[number];

/** GET /v1/price/batch's asset_ids cap (priceBatchMaxAssets, internal/api/v1/price.go). */
export const PRICE_BATCH_GET_MAX_IDS = 100;

/**
 * False for the ids the account-state surface serves as a trustline's
 * asset that name nothing the pricing API can parse: a classic AMM pool
 * share (`pool:<hex>`) and a trustline whose asset failed validation
 * (`unknown_asset`). Sending one 400s the whole batch request.
 */
export function isPriceableAssetId(id: string): boolean {
  return id !== '' && id !== 'unknown_asset' && !id.startsWith('pool:');
}

export interface ChunkedPriceBatch {
  rows: PriceBatchRow[];
  /** `flags.stale` OR-ed over the chunks that answered. */
  stale: boolean;
  /** Ids whose chunk was rejected — unanswered, not "no price". */
  failedIds: string[];
}

/**
 * Prices `ids` in ≤100-id GET chunks settled independently: the batch
 * rejects a whole request on one id it cannot parse, so a bad id costs
 * only its own chunk, and the caller learns which ids went unanswered.
 * GET (not POST) keeps each chunk edge-cacheable.
 */
export async function fetchPriceBatchChunked(
  ids: readonly string[],
  quote: string,
): Promise<ChunkedPriceBatch> {
  const chunks: string[][] = [];
  for (let i = 0; i < ids.length; i += PRICE_BATCH_GET_MAX_IDS) {
    chunks.push(ids.slice(i, i + PRICE_BATCH_GET_MAX_IDS));
  }
  const settled = await Promise.allSettled(
    chunks.map((chunk) =>
      apiGet<PriceBatchEnvelope>('/v1/price/batch', {
        asset_ids: chunk.join(','),
        quote,
      }),
    ),
  );
  const out: ChunkedPriceBatch = { rows: [], stale: false, failedIds: [] };
  settled.forEach((res, i) => {
    if (res.status === 'rejected') {
      out.failedIds.push(...chunks[i]);
      return;
    }
    out.rows.push(...(res.value.data ?? []));
    if (res.value.flags?.stale) out.stale = true;
  });
  return out;
}
