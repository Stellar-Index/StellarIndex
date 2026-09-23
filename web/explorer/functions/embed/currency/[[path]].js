// CF Pages Function for /embed/currency/* — serves the pre-rendered widgets
// first, else the client shell. Embeds are iframed by third-party sites
// that outlive any one build, so a ticker the build-time listing lacked
// (or a lowercase one; the build emits uppercase only) would otherwise
// hard-404 inside the customer's iframe (T291).
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/embed/currency/shell/');
}
