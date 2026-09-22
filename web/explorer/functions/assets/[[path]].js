// CF Pages Function for /assets/* — serves the pre-rendered pages first,
// else the client shell.
//
// 2026-08-05: /assets/[slug] pre-renders the top-500 listing + verified
// catalogue at build time. Migration 0134 gave EVERY classic asset
// (194k) a unique public slug, the API resolves them
// (/v1/assets/{slug}), and the listing links by slug — so any asset
// outside the pre-render snapshot hard-404'd on the static host (live
// report: /assets/usdt-gasu4kif). Same shell-fallback pattern as
// /markets, /accounts, /contracts, /issuers, /ledgers, /transactions
// (S-022 / S1b): pre-rendered slugs keep their SEO, everything else
// hydrates from the API via the /assets/shell/ client view.
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/assets/shell/');
}
