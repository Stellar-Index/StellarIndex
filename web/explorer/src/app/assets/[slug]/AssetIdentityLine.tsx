import { Mono } from '@/components/ui';

/** The asset's canonical id and its SAC address, each shortened with a copy button that copies the full value; XLM passes no id. */
export function AssetIdentityLine({
  identity,
  sac,
}: {
  identity: string | null;
  sac: string | null;
}) {
  const dash = identity?.indexOf('-') ?? -1;
  return (
    <dl className="text-ink-muted flex flex-wrap items-center gap-x-5 gap-y-1 text-xs">
      {identity && (
        <div className="flex items-center gap-1.5">
          <dt>Identity</dt>
          <dd>
            <Mono
              value={identity}
              truncate={dash > 0 ? { head: dash + 5, tail: 4 } : undefined}
              className="text-ink-body text-xs"
            />
          </dd>
        </div>
      )}
      {sac && (
        <div className="flex items-center gap-1.5">
          <dt>SAC</dt>
          <dd>
            <Mono
              value={sac}
              truncate={{ head: 5, tail: 5 }}
              className="text-ink-body text-xs"
            />
          </dd>
        </div>
      )}
    </dl>
  );
}
