# Link Log

A running index of every URL, GitHub discussion, or third-party resource the
user shares with us during discovery. Each entry records the link, the
context in which it came up, and where (if anywhere) we've captured the
useful content in a structured audit doc.

## Conventions

- **Status**: `raw` (not yet reviewed) · `in-progress` · `captured`
  (content extracted into a source doc) · `stale` (link dead or superseded).
- If a link contributes facts to an audit doc, link to that doc in **Captured
  to**.
- Keep user-provided quotes verbatim in **Quote**; don't paraphrase them away.

## Entries

### 2026-04-22

| # | Link | Context | Status | Captured to |
| - | ---- | ------- | ------ | ----------- |
| 1 | https://github.com/orgs/stellar/discussions/1872 | First link shared during Phase 1 kickoff. Relevance TBD. | raw | — |
| 2 | https://github.com/stellar/go-stellar-sdk/tree/main/tools/stellar-archivist | `stellar-archivist` tool for history archive manipulation. NB: path may actually live in `stellar/go` rather than `stellar/go-stellar-sdk`; verify. | raw | (will go to `data-sources/stellar-archivist.md`) |
| 3 | https://developers.stellar.org/docs/validators/admin-guide/publishing-history-archives#complete-history-archive | Official Stellar docs on publishing complete history archives — relevant to self-hosting full archival nodes. | raw | (will go to `data-sources/archival-nodes.md`) |

### Notes / Quotes

> **User (re: `stellar-archivist`, 2026-04-22):** "Still checking around,
> but using mirror instead of repair is, I believe, faster for ingestion
> (and can be sped up more with `-c`)."

Action: capture `mirror` vs `repair` trade-off and the `-c` (concurrency)
flag in the archivist audit doc, with a test plan to confirm the claim on
our own hardware.
