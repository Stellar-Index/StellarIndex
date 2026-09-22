// CF Pages Function for /issuers/* — serves the pre-rendered top-100
// pages first, else the client shell (site audit S-022: issuers beyond
// the top-100 hard-404'd while search + asset pages linked to them).
// See functions/transactions/[[path]].js for the full rationale.
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/issuers/shell/');
}
