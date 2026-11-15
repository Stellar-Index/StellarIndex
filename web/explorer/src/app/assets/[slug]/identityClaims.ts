// What the asset page may say about identity the issuer asserts for
// itself. A ticker and a home domain are both self-set.

/**
 * homeDomainLabel — the home domain is the issuer's own account field.
 * Only a stellar.toml at that domain listing this (code, issuer) makes
 * it more than a claim.
 */
export function homeDomainLabel(sep1Status?: string | null): string {
  return sep1Status === 'verified' ? 'Home domain' : 'Claimed home domain';
}

/**
 * lookAlikeMeta — title and description for an asset that wears a
 * verified asset's ticker from another issuer: the issuer is named and
 * no price is quoted, so a shared link cannot pass for the real asset.
 */
export function lookAlikeMeta(
  code: string,
  issuer?: string | null,
): { title: string; description: string } {
  const who = issuer ? `${issuer.slice(0, 4)}…${issuer.slice(-4)}` : 'unknown';
  return {
    title: `${code} (issuer ${who}) — unverified look-alike`,
    description: `This asset uses the ticker ${code} but is not the verified ${code} on Stellar. Issuer ${issuer ?? 'unknown'}.`,
  };
}
