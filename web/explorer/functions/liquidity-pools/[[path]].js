// CF Pages Function for /liquidity-pools/* — serves real assets first (the
// /liquidity-pools/ index), else the pool shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1).
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/liquidity-pools/shell/');
}
