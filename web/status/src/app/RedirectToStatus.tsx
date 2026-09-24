'use client';

import { useEffect } from 'react';

// Client-side belt-and-suspenders redirect for the rare case a visitor
// reaches this stub without the Cloudflare _redirects 301 firing (the
// edge rule is the primary path). Mirrors the edge rule's `:splat`
// behaviour by forwarding whatever came after the root, so an
// incident deep-link still lands on the right page even when the
// edge rule is bypassed.
export function RedirectToStatus({ target }: { target: string }) {
  useEffect(() => {
    const { pathname, search, hash } = window.location;
    const splat = pathname.replace(/^\/+/, '');
    window.location.replace(target + splat + search + hash);
  }, [target]);
  return null;
}
