// CF Pages Function for /contracts/* — serves real assets first (the /contracts/
// directory, or future pre-rendered active contracts), else the shell. See
// functions/transactions/[[path]].js for the full rationale (SEO plan D1).
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/contracts/shell/');
}
