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

export type ResolvedScamFlags = ScamFlags & {
  /** True when the live read failed or answered stale. */
  liveUnchecked: boolean;
};

/**
 * The issuer's scam flags on a pre-rendered page, for every consumer.
 * The live read may add a flag but never clears a baked one: the payload
 * omits the tags both for "not flagged" and for a failed directory lookup
 * (that failure is not on the JSON wire), so absence is not a clean answer.
 */
export function useIssuerScamFlags(
  assetID: string,
  baked: ScamFlags,
): ResolvedScamFlags {
  const q = useAsset(assetID);
  const live = q.data?.data;
  const tags = [
    ...new Set([
      ...(baked.directoryTags ?? []),
      ...(live?.issuer_directory_tags ?? []),
    ]),
  ];
  return {
    directoryTags: tags,
    scamReason: live?.issuer_scam_reason?.trim() || baked.scamReason,
    directoryDomain: live?.issuer_directory_domain || baked.directoryDomain,
    liveUnchecked: q.isError || !!q.data?.flags?.stale,
  };
}

/** The banner on a pre-rendered page, seeded by the baked flags. */
export function LiveAssetScamCallout({
  assetID,
  ...baked
}: ScamFlags & { assetID: string }) {
  const { directoryTags, scamReason, directoryDomain } = useIssuerScamFlags(
    assetID,
    baked,
  );
  return (
    <AssetScamCallout
      directoryTags={directoryTags}
      scamReason={scamReason}
      directoryDomain={directoryDomain}
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
