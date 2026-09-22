// CF Pages Function for /insights/creators/* — serves real assets first (the
// /insights/creators/ board itself), else the one built per-address shell.
// See functions/transactions/[[path]].js for the full rationale (SEO plan D1).
//
// The per-address route is a single `shell` document because no build can
// enumerate the address set behind it; /creator/{g} 301s here via
// public/_redirects, so this function is what makes that alias resolve
// rather than 404 on the long tail.
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/insights/creators/shell/');
}
