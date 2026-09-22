// CF Pages Function for /lending/* — serves the pre-rendered pages first,
// else the client shell.
//
// T278: web/explorer/src/app/lending/[pool] pre-renders every pool
// /v1/lending/pools currently lists plus the curated Blend
// factory/backstop contracts at BUILD time — but this directory did not
// exist, so a pool the Blend factory spawns between builds hard-404'd on
// the static host with no fallback. Same shell-fallback pattern already
// used by /assets, /markets, /accounts, /contracts, /issuers, /ledgers,
// /transactions (S-022 / S1b): pre-rendered pools keep their SEO,
// everything else hydrates from the API via LendingPoolPathView.
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/lending/shell/');
}
