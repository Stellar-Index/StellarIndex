# Evidence

This directory holds every piece of evidence collected during the
audit. Evidence is split across ledgers, each with its own ID
prefix (per [02-protocol.md](../02-protocol.md) §4).

| File | Prefix | Content |
| --- | --- | --- |
| [log.md](log.md) | `EV-####` | code/test/runtime observations |
| [commands.md](commands.md) | `CMD-####` | shell command transcripts (exact output) |
| [cross-file-interactions.md](cross-file-interactions.md) | `XFI-####` | material seams + class-roll-up |
| [tree-observations.md](tree-observations.md) | `TREE-####` | filesystem-tree observations |
| [r1-probes/](r1-probes/) | `R1-####` | live R1 SSH probe transcripts |
| [journeys/](journeys/) | (per-journey supporting evidence) | — |
| [workstreams/](workstreams/) | (per-workstream supporting evidence) | — |

## Discipline

- IDs are monotonic per ledger. Do not reuse IDs.
- Invalidated entries: mark `superseded by <NEW-ID>` instead of
  deleting.
- Every entry: ID, UTC ISO-8601 date, claim or observation,
  source refs, workstream(s), notes (≤200 chars).
- A finding can cite IDs from any ledger.
- Per memory `feedback_r1_sql_quoting`: for any SQL over SSH,
  use `scp` + `psql -f`; never inline `$$..$$`.
- Per memory `feedback_no_pipe_through_tail`: never pipe a gate
  through `tail` (masks exit codes).
