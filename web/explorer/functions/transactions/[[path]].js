// Cloudflare Pages Function for /transactions/* (the unbounded transaction
// entity routes). SEO plan D1; required because Spike A (2026-06-24) proved a
// `_redirects` 200-rewrite catch-all CLOBBERS real static files — so we serve
// the shell from a Function that explicitly defers to static assets first.
//
//   1. Try the real pre-rendered asset (the /transactions/ list page, or any
//      future curated /transactions/{hash} page). Serve it if present.
//   2. Otherwise serve the static shell (/transactions/shell/) with the
//      requested URL preserved (200, not a soft-404). The shell's client reads
//      the real hash from window.location.pathname (TxPathView) and fetches it.
//
// `env.ASSETS.fetch` hits the static-asset store directly (it does NOT
// re-invoke Functions), so there is no recursion.
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/transactions/shell/');
}
