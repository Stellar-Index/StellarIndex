'use client';

import { useAsset } from '@/api/hooks';
import { Callout } from '@/components/ui';
import { scamFlagTags } from '@/lib/directory-tags';

/**
 * The scam/malicious warning banner for an asset, shared by BOTH asset
 * render paths.
 *
 * It has to be shared because it was not. The banner lived inline in the
 * pre-rendered asset page, and `/assets/[slug]` has a SECOND path: only
 * the top-500 assets are pre-rendered, and everything else falls through
 * to the client shell (AssetPathView). The shell fetches the same
 * `/v1/assets/{id}` payload — carrying the same `issuer_directory_tags`
 * and `issuer_scam_reason` — and rendered no warning at all. So the
 * long-tail assets, which is where a scam token actually sits, were the
 * ones served without the warning.
 *
 * Keeping one component means the two paths cannot drift again, and the
 * guard in `src/lib/trust-surface-guards.test.ts` fails if an asset view
 * is added that does not import it.
 *
 * Renders nothing when the asset carries no scam-class flag, so callers
 * can mount it unconditionally.
 */
type ScamFlags = {
  directoryTags?: readonly string[] | null;
  scamReason?: string | null;
  directoryDomain?: string | null;
};

/**
 * The banner on a pre-rendered page: the baked flags are only a seed, and
 * the live read supersedes them, so an issuer flagged after the build
 * still gets the warning.
 */
export function LiveAssetScamCallout({
  assetID,
  ...baked
}: ScamFlags & { assetID: string }) {
  const live = useAsset(assetID).data?.data;
  return (
    <AssetScamCallout
      {...(live
        ? {
            directoryTags: live.issuer_directory_tags,
            scamReason: live.issuer_scam_reason,
            directoryDomain: live.issuer_directory_domain,
          }
        : baked)}
    />
  );
}

export function AssetScamCallout({
  directoryTags,
  scamReason,
  directoryDomain,
}: ScamFlags) {
  const flaggedDirTags = scamFlagTags(directoryTags);
  const reason = scamReason?.trim() || null;
  if (!reason && flaggedDirTags.length === 0) return null;

  return (
    <Callout
      tone="bad"
      title={reason ? 'Known scam asset' : 'Flagged by community directory'}
    >
      {reason && <p>{reason}.</p>}
      <p className={reason ? 'mt-1' : undefined}>
        Flagged by the stellar.expert community directory
        {flaggedDirTags.length > 0 && (
          <>
            {' '}
            as{' '}
            <strong className="font-semibold">
              {flaggedDirTags.join(', ')}
            </strong>
          </>
        )}
        {directoryDomain && <> ({directoryDomain})</>}.
      </p>
      <p className="mt-1 font-medium">Do not trust this asset or its prices.</p>
    </Callout>
  );
}
