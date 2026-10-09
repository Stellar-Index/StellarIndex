// CF Pages Function for /convert/*. Pairs are baked upper-case only, so a
// lower-case or alias slug 301s to its baked page; a pair with no baked
// page 302s to the landing picker with that pair preselected.
import { parseConvertPath } from '../_shared/convertSlug.js';
import {
  permanentRedirect,
  redirect,
  serveAsset,
} from '../_shared/shellFallback.js';

export async function onRequest(context) {
  const url = new URL(context.request.url);
  const pair = parseConvertPath(url.pathname);
  if (!pair) return serveAsset(context);

  try {
    const baked = await context.env.ASSETS.fetch(
      new Request(new URL(pair.canonical, url.origin), { method: 'HEAD' }),
    );
    if (baked.status !== 404) {
      if (url.pathname === pair.canonical) return serveAsset(context);
      return permanentRedirect(`${pair.canonical}${url.search}`);
    }
  } catch {
    return serveAsset(context);
  }
  const q = new URLSearchParams({ from: pair.from, to: pair.to });
  return redirect(`/convert/?${q}`, 302);
}
