/** Layout pieces shared by the methodology, terms, SLA and privacy pages. */
export function Section({
  id,
  title,
  subtitle,
  children,
}: {
  id: string;
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <section id={id} className="scroll-mt-24 space-y-4">
      <header className="space-y-1">
        <h2 className="text-h3 text-ink font-semibold">
          <a
            href={`#${id}`}
            className="hover:text-brand-600"
            aria-label={`Anchor to ${title}`}
          >
            {title}
          </a>
        </h2>
        {subtitle && <p className="text-ink-muted text-sm">{subtitle}</p>}
      </header>
      <div className="text-ink-body space-y-3 text-sm leading-6">
        {children}
      </div>
    </section>
  );
}

export function TableOfContents({
  toc,
}: {
  toc: { id: string; label: string }[];
}) {
  return (
    <nav className="border-line bg-surface rounded-card border p-4">
      <h2 className="text-h3 text-ink mb-2 font-semibold">Contents</h2>
      <ol className="space-y-1 text-sm">
        {toc.map((t, i) => (
          <li key={t.id}>
            <a href={`#${t.id}`} className="text-ink-body hover:text-brand-600">
              {i + 1}. {t.label}
            </a>
          </li>
        ))}
      </ol>
    </nav>
  );
}

export function DefList({ rows }: { rows: { term: string; def: string }[] }) {
  return (
    <dl className="space-y-3">
      {rows.map((r) => (
        <div
          key={r.term}
          className="grid grid-cols-1 gap-1 sm:grid-cols-[10rem_1fr] sm:gap-3"
        >
          <dt className="text-brand-600 min-w-0 font-mono text-xs font-semibold [overflow-wrap:anywhere]">
            {r.term}
          </dt>
          <dd>{r.def}</dd>
        </div>
      ))}
    </dl>
  );
}

export function Aside({ children }: { children: React.ReactNode }) {
  return (
    <p className="border-line bg-surface-subtle text-ink-body rounded-md border px-3 py-2 text-xs">
      {children}
    </p>
  );
}

export function PolicyHistory({
  history,
}: {
  history: { date: string; note: string }[];
}) {
  return (
    <section id="history" className="scroll-mt-24 space-y-2">
      <h2 className="text-h3 text-ink font-semibold">Policy history</h2>
      <ul className="text-ink-body space-y-1 text-sm">
        {history.map((h) => (
          <li key={h.date}>
            <span className="font-mono text-xs">{h.date}</span> — {h.note}
          </li>
        ))}
      </ul>
    </section>
  );
}
