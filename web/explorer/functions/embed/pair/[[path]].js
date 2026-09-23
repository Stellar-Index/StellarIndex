// CF Pages Function for /embed/pair/* — serves the pre-rendered widgets
// first, else the client shell. The build pre-renders only the top 100
// markets by volume; any other pair iframed by a third-party site would
// otherwise hard-404 inside the customer's iframe (T291).
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/embed/pair/shell/');
}
