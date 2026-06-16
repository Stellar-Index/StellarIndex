import type { ADRStatus } from '@/lib/adr';

const STYLES: Record<ADRStatus, string> = {
  Accepted: 'bg-emerald-100 text-emerald-700',
  Proposed: 'bg-amber-100 text-amber-700',
  Superseded: 'bg-surface-subtle text-ink-body',
  Rejected: 'bg-rose-100 text-rose-700',
};

export function StatusBadge({ status }: { status: ADRStatus }) {
  return (
    <span
      className={`inline-flex items-center rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider ${STYLES[status] ?? STYLES.Proposed}`}
    >
      {status}
    </span>
  );
}
