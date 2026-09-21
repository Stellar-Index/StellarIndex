// Validation for issuer home_domain values before they are
// rendered as clickable links.
//
// home_domain is attacker-controlled on-chain data (any account can
// set an AccountEntry home_domain to an arbitrary 32-byte string).
// Rendering it as a bare `https://<home_domain>` <a> without
// validation is a phishing surface: a scammer can set
// home_domain = "evil.example.com/login?next=" or smuggle a
// userinfo segment ("good.com@evil.com") so the rendered link text
// looks legitimate while the resolved origin is attacker-owned.
//
// isSafeHomeDomain accepts only a strict hostname: lowercase
// letters, digits, dots and hyphens, with at least one dot (so a
// real registrable domain, not a bare label or an IP-with-port).
// No `@`, no `/`, no `:`, no whitespace, no scheme. Anything that
// fails is rendered as plain text by the caller instead of a link.
const HOSTNAME_RE =
  /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/;

export function isSafeHomeDomain(
  domain: string | undefined | null,
): domain is string {
  if (!domain) return false;
  if (domain.length > 253) return false;
  // Reject anything with structural URL characters up front — the
  // regex below would already reject these, but being explicit
  // documents the threat (userinfo `@`, path `/`, scheme `:`).
  if (/[@/\s:]/.test(domain)) return false;
  return HOSTNAME_RE.test(domain);
}

// SEC-10: validation for issuer-controlled SEP-1 `image` URLs before they
// are rendered as an <img src>. image is issuer-controlled (any asset
// issuer's stellar.toml CURRENCIES[].image); scheme-only validation
// (https?://…) lets a hostile issuer point every viewer's browser at an
// arbitrary host — an internal/private address on the VIEWER's own
// network (client-side SSRF against their router, or a cloud metadata
// endpoint for automated screenshot/preview bots), not just a normal
// third-party tracking beacon.
//
// This is necessarily host-string validation only: JS can't resolve DNS
// before the browser's own `<img>` fetch, so a public hostname that's
// later repointed (DNS rebinding) isn't caught here — closing that
// requires routing icons through a same-origin proxy (tracked in
// public/_headers' img-src TODO), which is a separate, larger change.
// This closes the straightforward case: an issuer setting `image` to a
// literal private/loopback/link-local IP or `localhost`.
const IPV4_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

function isPrivateIPv4(host: string): boolean {
  const m = IPV4_RE.exec(host);
  if (!m) return false;
  const [a, b] = [Number(m[1]), Number(m[2])];
  if ([a, b, Number(m[3]), Number(m[4])].some((n) => n > 255)) return false;
  if (a === 127) return true; // loopback
  if (a === 10) return true; // RFC1918
  if (a === 172 && b >= 16 && b <= 31) return true; // RFC1918
  if (a === 192 && b === 168) return true; // RFC1918
  if (a === 169 && b === 254) return true; // link-local incl. cloud metadata (169.254.169.254)
  if (a === 100 && b >= 64 && b <= 127) return true; // CGNAT
  if (a === 0) return true; // "this" network
  return false;
}

// An IPv4-mapped IPv6 literal (::ffff:a.b.c.d, or its compressed hex form
// such as ::ffff:7f00:1 that `new URL(...).hostname` normalizes to) targets
// the same host at the network layer as the embedded IPv4 address. Returns
// that IPv4 in dotted form, or null if host isn't one of these.
function ipv4FromMappedIPv6(host: string): string | null {
  const dotted = /^::ffff:(\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3})$/.exec(host);
  if (dotted) return dotted[1];
  const hex = /^::ffff:([0-9a-f]{1,4}):([0-9a-f]{1,4})$/.exec(host);
  if (!hex) return null;
  const n1 = parseInt(hex[1], 16);
  const n2 = parseInt(hex[2], 16);
  return `${n1 >> 8}.${n1 & 0xff}.${n2 >> 8}.${n2 & 0xff}`;
}

function isPrivateHostname(hostname: string): boolean {
  const host = hostname
    .toLowerCase()
    .replace(/^\[|\]$/g, '') // strip IPv6 [] brackets
    .replace(/\.$/, ''); // normalize a trailing root-zone dot ("localhost.")
  if (host === 'localhost' || host.endsWith('.localhost')) return true;
  if (host.endsWith('.local') || host.endsWith('.internal')) return true;
  if (isPrivateIPv4(host)) return true;
  const mapped = ipv4FromMappedIPv6(host);
  if (mapped && isPrivateIPv4(mapped)) return true;
  // IPv6 loopback, unspecified, unique-local (fc00::/7), link-local (fe80::/10).
  if (host === '::1' || host === '::') return true;
  if (/^(fc|fd)[0-9a-f]{2}:/.test(host)) return true;
  if (/^fe[89ab][0-9a-f]:/.test(host)) return true;
  return false;
}

/**
 * isSafePublicImageUrl — https-only, hostname-validated check for a
 * remote issuer-supplied image URL. Reject scheme !== https, missing
 * host, or a private/loopback/link-local/localhost host.
 */
export function isSafePublicImageUrl(
  url: string | undefined | null,
): url is string {
  if (!url) return false;
  let parsed: URL;
  try {
    parsed = new URL(url);
  } catch {
    return false;
  }
  if (parsed.protocol !== 'https:') return false;
  if (!parsed.hostname) return false;
  if (isPrivateHostname(parsed.hostname)) return false;
  return true;
}
