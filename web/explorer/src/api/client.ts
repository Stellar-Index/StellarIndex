// Thin fetch wrapper for the Stellar Index API.
//
// Resolves the base URL from `NEXT_PUBLIC_API_BASE_URL` (set in
// `next.config.mjs`). Use this everywhere instead of constructing
// URLs by hand so the `<>` reveal can introspect every request.

export const API_BASE_URL =
  process.env.NEXT_PUBLIC_API_BASE_URL ?? 'https://api.stellarindex.io';

// [absence: timeouts] Every runtime (client-side) fetch used to have no
// upper bound at all — a hung connection left a query (and anything
// gating on it, e.g. AccountGate) in "loading" forever with no escape
// hatch. 15s is generous for a live-data round trip but still bounded.
const DEFAULT_FETCH_TIMEOUT_MS = 15_000;

/**
 * timeoutSignal — an AbortSignal that fires on whichever comes first: the
 * given timeout, or (when provided) an external signal aborting first
 * (e.g. TanStack Query cancelling a superseded/unmounted request). Use
 * this instead of passing an external signal straight through, so every
 * request keeps a hard upper bound even when the caller doesn't think to
 * set one.
 */
export function timeoutSignal(
  ms: number = DEFAULT_FETCH_TIMEOUT_MS,
  external?: AbortSignal,
): AbortSignal {
  if (!external) return AbortSignal.timeout(ms);
  const controller = new AbortController();
  const abort = (reason: unknown) => controller.abort(reason);
  if (external.aborted) {
    abort(external.reason);
  } else {
    external.addEventListener('abort', () => abort(external.reason), {
      once: true,
    });
  }
  const timer = setTimeout(
    () => abort(new DOMException('Request timed out', 'TimeoutError')),
    ms,
  );
  controller.signal.addEventListener('abort', () => clearTimeout(timer), {
    once: true,
  });
  return controller.signal;
}

export type RequestExample = {
  method: 'GET' | 'POST';
  url: string;
  headers?: Record<string, string>;
};

function buildUrl(
  path: string,
  params?: Record<string, string | number | undefined>,
): string {
  const url = new URL(path.startsWith('/') ? path : `/${path}`, API_BASE_URL);
  if (params) {
    for (const [k, v] of Object.entries(params)) {
      if (v !== undefined) url.searchParams.set(k, String(v));
    }
  }
  return url.toString();
}

export async function apiGet<T>(
  path: string,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  const res = await fetch(buildUrl(path, params), {
    headers: { Accept: 'application/json' },
    next: { revalidate: 60 },
    signal: timeoutSignal(),
  });
  if (!res.ok) {
    // Surface the RFC-9457 problem `title` (and `detail`) in the error so
    // callers can distinguish failure modes that share a status code — e.g.
    // a /wasm 404 that is "SAC, no WASM" vs "not captured yet". The status
    // text is kept first so existing `.includes('404')` checks still match.
    let extra = '';
    try {
      const body = (await res.json()) as { title?: string; detail?: string };
      if (body?.title) extra = ` — ${body.title}`;
      if (body?.detail) extra += `${extra ? ':' : ' —'} ${body.detail}`;
    } catch {
      /* non-JSON body — keep the bare status line */
    }
    throw new Error(`${res.status} ${res.statusText} on ${path}${extra}`);
  }
  return (await res.json()) as T;
}

/**
 * Envelope — the standard /v1 response wrapper (FEC audit A3-F4:
 * re-homed here from app/explorer-shared so src/api code can use it,
 * and extended with `pagination` — its absence was exactly why the
 * pager tables each inlined a private envelope type).
 */
export type Envelope<T> = {
  data: T;
  as_of?: string;
  flags?: Record<string, unknown>;
  pagination?: { next?: string };
};

/**
 * apiGetData — apiGet + the `.data` unwrap that ~28 call sites
 * re-spelled (`(await apiGet<Envelope<T>>(p)).data`). Sites that need
 * `pagination` / `as_of` / `flags` keep envelope form via apiGet.
 */
export async function apiGetData<T>(
  path: string,
  params?: Record<string, string | number | undefined>,
): Promise<T> {
  return (await apiGet<Envelope<T>>(path, params)).data;
}

// Helper for the <> reveal. Every panel exports a getRequestExample()
// that returns this shape; the reveal renders it as cURL + clickable URL.
export function asExample(
  path: string,
  params?: Record<string, string | number | undefined>,
): RequestExample {
  return { method: 'GET', url: buildUrl(path, params) };
}
