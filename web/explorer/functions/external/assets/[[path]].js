// CF Pages Function for /external/assets/* — serves the pre-rendered
// reference-asset pages (and the /external/assets hub) first, else the
// client shell. The build enumerates /v1/external/assets once; a slug added
// since would otherwise hard-404 on the static host (T291). See
// ExternalAssetPathView.
import { shellFallback } from '../../_shared/shellFallback.js';

export async function onRequest(context) {
  return shellFallback(context, '/external/assets/shell/');
}
