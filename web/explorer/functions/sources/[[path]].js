// CF Pages Function for /sources/* — serves the pre-rendered source pages
// first, else the client shell. /sources/[name] pre-renders only the names
// /v1/sources listed at build time; a source registered since would
// otherwise hard-404 on the static host (T291). See SourcePathView.
import { shellFallback } from '../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/sources/shell/');
}
