// CF Pages Function for /embed/asset/* — serves the pre-rendered widgets
// first, else the client shell. Embeds are iframed by third-party sites
// that outlive any one build, so a slug the build-time listing lacked
// would otherwise hard-404 inside the customer's iframe (T291).
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/embed/asset/shell/');
}
