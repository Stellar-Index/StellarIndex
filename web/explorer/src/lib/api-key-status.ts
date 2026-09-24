import type { APIKey } from '@/api/account';

export type KeyStatus = 'active' | 'expired' | 'revoked';

/**
 * The one dashboard rule for a key's state, matching the server's
 * platform.APIKey.IsActive: revoked wins, and a key is expired from the
 * instant `expires_at` is reached. Only 'active' keys authenticate; the
 * key cap counts every non-revoked key, so an expired key still holds a
 * slot until it is revoked.
 */
export function keyStatus(
  k: Pick<APIKey, 'revoked_at' | 'expires_at'>,
  now: Date = new Date(),
): KeyStatus {
  if (k.revoked_at) return 'revoked';
  if (k.expires_at && now.getTime() >= new Date(k.expires_at).getTime()) {
    return 'expired';
  }
  return 'active';
}

/** True when the key authenticates right now. */
export function isKeyLive(
  k: Pick<APIKey, 'revoked_at' | 'expires_at'>,
  now: Date = new Date(),
): boolean {
  return keyStatus(k, now) === 'active';
}
