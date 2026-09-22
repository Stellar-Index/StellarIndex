// CF Pages Function for /insights/sponsors/* — serves real assets first (the
// /insights/sponsors/ board itself), else the one built per-address shell.
// See functions/transactions/[[path]].js for the full rationale (SEO plan D1).
//
// The per-address route is a single `shell` document because no build can
// enumerate the address set behind it; /sponsor/{g} 301s here via
// public/_redirects, so this function is what makes that alias resolve
// rather than 404 on the long tail.
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/insights/sponsors/shell/');
}
