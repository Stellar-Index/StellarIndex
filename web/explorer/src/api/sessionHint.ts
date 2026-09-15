'use client';

import { useSyncExternalStore } from 'react';

/**
 * The session hint — a JS-readable presence flag the API writes beside
 * the HttpOnly session cookie (`internal/api/v1/dashboardauth/session_hint.go`).
 *
 * The explorer and the API are separate origins, so the only way this
 * app could tell a signed-in visitor from an anonymous one was to make
 * a credentialed `GET /v1/account/me` and read the status back. For an
 * anonymous visitor that is a guaranteed 401 on every page load, plus a
 * refetch every five minutes — and the browser writes its own console
 * entry for any non-2xx, from the network layer, where no `catch` can
 * reach it. The hint lets an anonymous visitor make the request zero
 * times instead.
 *
 * Three properties matter:
 *
 *  - It is a HINT. Nothing is authorized by it; the session cookie is
 *    still the credential, still HttpOnly, still checked server-side on
 *    every request. Forging this cookie buys an attacker one 401.
 *  - Its absence is treated as "signed out", which is also what an
 *    older API that doesn't set it yet produces. That degrades to the
 *    signed-out CTAs — never to a broken signed-in state.
 *  - It can outlive the real session (server-side expiry, revocation,
 *    one side cleared without the other). The probe then runs and 401s
 *    exactly as it does today, and `useMe` drops the stale hint so the
 *    next page load is quiet again.
 */
export const SESSION_HINT_COOKIE = 'stellarindex_session_present';

/** Subscribers to local hint changes (i.e. clearSessionHint). */
const listeners = new Set<() => void>();

function subscribe(onStoreChange: () => void): () => void {
  listeners.add(onStoreChange);
  return () => {
    listeners.delete(onStoreChange);
  };
}

/**
 * Reads one cookie out of a `document.cookie` jar string. An empty
 * value reads as absent: a cookie cleared with `name=` rather than an
 * expiry would otherwise look like a live session.
 */
export function readCookie(jar: string, name: string): string | null {
  for (const part of jar.split(';')) {
    const eq = part.indexOf('=');
    if (eq < 0) continue;
    if (part.slice(0, eq).trim() !== name) continue;
    const value = part.slice(eq + 1).trim();
    return value === '' ? null : value;
  }
  return null;
}

/** Whether this browser is holding a session hint right now. */
export function sessionHintPresent(): boolean {
  if (typeof document === 'undefined') return false;
  return readCookie(document.cookie, SESSION_HINT_COOKIE) !== null;
}

/**
 * The domain attributes a deletion has to be attempted with. Removing a
 * cookie requires the same Domain and Path it was set with, and this
 * app does not know the API's configured `cookie_domain` — so try the
 * host-only form and then every parent up to (not past) the registrable
 * pair. The attempts that don't match are inert, and a browser rejects
 * outright any that names a public suffix.
 */
function candidateDomains(hostname: string): (string | undefined)[] {
  const candidates: (string | undefined)[] = [undefined];
  const labels = hostname.split('.');
  for (let i = 0; i <= labels.length - 2; i++) {
    candidates.push(labels.slice(i).join('.'));
  }
  return candidates;
}

/**
 * Drops a hint that has outlived its session, so the next page load
 * makes no credentialed request at all.
 */
export function clearSessionHint(): void {
  if (typeof document === 'undefined') return;
  const secure = window.location.protocol === 'https:' ? '; Secure' : '';
  for (const domain of candidateDomains(window.location.hostname)) {
    document.cookie =
      `${SESSION_HINT_COOKIE}=; Max-Age=0; Path=/; SameSite=Lax` +
      (domain ? `; Domain=${domain}` : '') +
      secure;
  }
  for (const listener of listeners) listener();
}

/** Server/prerender snapshot — a static export has no cookie jar. */
function absent(): boolean {
  return false;
}

/**
 * useSessionHint — re-renders when the hint is dropped, so a query
 * gated on it stops refetching the moment it goes stale rather than at
 * the next accidental render.
 */
export function useSessionHint(): boolean {
  return useSyncExternalStore(subscribe, sessionHintPresent, absent);
}
