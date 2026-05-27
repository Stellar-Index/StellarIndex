# Tree Observations

Filesystem-tree observations: directory shapes, file counts,
ownership patterns, unexpected residue.

| ID | Date | Observation | Source | Workstream | Notes |
| --- | --- | --- | --- | --- | --- |
| TREE-0001 | 2026-05-26 | 2104 tracked files in repo (baseline 2026-05-12 had ~1700+) | git ls-files | W01 | scope grew ~25% |
| TREE-0002 | 2026-05-26 | 23 source packages under `internal/sources/` (was 18; +cctp +defindex +rozo +sorobanevents +soroswap_router) | ls | W07 | |
| TREE-0003 | 2026-05-26 | 11 external adapters under `internal/sources/external/` (was 10; +chainlink) | ls | W08 | |
| TREE-0004 | 2026-05-26 | 45 .up.sql migrations (was 28) | ls | W09 | |
| TREE-0005 | 2026-05-26 | 50 handler files under `internal/api/v1/` | ls | W11 | |
| TREE-0006 | 2026-05-26 | 387 *_test.go files | git ls-files | W15 | |
| TREE-0007 | 2026-05-26 | 28 integration tests under `test/integration/` (was 19) | ls | W15 | |
| TREE-0008 | 2026-05-26 | 40 timescale storage files under `internal/storage/timescale/` | ls | W09 | |
| TREE-0009 | 2026-05-26 | 29 ADRs under `docs/adr/` (was 26; +0027 +0028 +0029) | ls | W02 | |
| TREE-0010 | 2026-05-26 | 10 workflows under `.github/workflows/` | ls | W03 | |
| TREE-0011 | 2026-05-26 | 195846 total Go LOC | wc | meta | scope reference |
