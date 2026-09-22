// CF Pages Function for /ledgers/* — serves real assets first (the /ledgers/
// list, or future pre-rendered recent ledgers), else the shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1;
// Spike A proved a _redirects catch-all clobbers static files).
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/ledgers/shell/');
}
