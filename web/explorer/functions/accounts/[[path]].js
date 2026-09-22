// CF Pages Function for /accounts/* — serves real assets first (the /accounts/
// richlist, or future pre-rendered curated accounts), else the shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1).
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/accounts/shell/');
}
