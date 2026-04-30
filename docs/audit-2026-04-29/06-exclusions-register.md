# Exclusions and Assumptions Register

| ID | Type | Scope item | Reason | Temporary? | Needed to clear exclusion | Evidence refs | Notes |
| --- | --- | --- | --- | --- | --- | --- | --- |
| X-0001 | assumption | Hosted GitHub branch protection and required checks | Local checkout cannot prove repository settings by itself | yes | GitHub settings/API evidence | — | Workflow files and policy docs are still audited locally. |
| X-0002 | exclusion | `docs/audit-2026-04-29/**` as product-behavior target | Audit workspace files are control artifacts for the audit itself, not application/runtime behavior under audit | no | None | [README.md](/Users/ash/code/ratesengine/docs/audit-2026-04-29/README.md:1) | Still mark them `done` in file coverage after confirming workspace integrity. |
| X-0003 | exclusion | `docs/discovery/**` as live-runtime truth target | Repo policy marks the discovery tree as a frozen read-only archive, not an actively refreshed current-state surface | no | None | `EV-0036` | Discovery docs are still cited where current code/docs explicitly depend on them, but they are not audited file-by-file as live product behavior. |
| X-0004 | exclusion | `docs/reference/api/index.html` generated render output | Generated artifact; source of truth is `openapi/rates-engine.v1.yaml` plus the short generated README | no | None | `EV-0037` | The OpenAPI spec and generated-doc pipeline are audited instead of the rendered HTML payload line-by-line. |
| X-0005 | exclusion | Raw JSON fixture captures under `test/fixtures/**` | Captured regression corpora, not independent logic-bearing runtime surfaces | no | None | `EV-0038` | Coverage comes from the owning decoder/package tests and fixture README metadata rather than line-by-line review of each JSON blob. |
