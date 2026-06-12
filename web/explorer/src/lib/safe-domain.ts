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
const HOSTNAME_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/;

export function isSafeHomeDomain(domain: string | undefined | null): domain is string {
  if (!domain) return false;
  if (domain.length > 253) return false;
  // Reject anything with structural URL characters up front — the
  // regex below would already reject these, but being explicit
  // documents the threat (userinfo `@`, path `/`, scheme `:`).
  if (/[@/\s:]/.test(domain)) return false;
  return HOSTNAME_RE.test(domain);
}
