# W23 — Multi-region determinism (R2, R3)

## Scope

Cross-region invariants. ADR-0015 (last-closed-bucket-rate-serving),
ADR-0016 (per-region storage strategy).

## Inputs

- ADR-0015, ADR-0016
- `docs/operations/r2-deployment-state.md`,
  `r3-deployment-state.md`
- `cmd/ratesengine-ops/cross_region_check.go`,
  `cross_region_monitor.go`
- `scripts/dev/verify-cross-region.sh`
- `docs/operations/multi-region-cutover.md`
- `docs/architecture/ha-plan.md`,
  `docs/architecture/infrastructure/multi-region-topology.md`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W23.1 | ADR-0015: closed-bucket-only contract enforced in every region's API | per-handler |
| W23.2 | ADR-0016: per-region storage strategy documented vs reality (R1 full mirror; R2 aws-public-blockchain S3 direct; R3 Vultr hybrid) | doc + code |
| W23.3 | R2 / R3 deployment-state docs accurate (X-0001: only R1 is live; R2/R3 absent — verify their docs say so) | doc |
| W23.4 | cross_region_check: drift definition + threshold + alert | code |
| W23.5 | cross_region_monitor: continuous monitoring + recovery action | code |
| W23.6 | verify-cross-region.sh: integration test path | shell |
| W23.7 | multi-region-cutover.md: step-by-step + rollback | doc |
| W23.8 | NEW: cold-tier (ADR-0027) interacts with multi-region (R2 already cold-reads from aws-public-blockchain; R3 hybrid) | cross-ref W30 |
| W23.9 | NEW: r2-r3-bringup.md (NEW since baseline) actionable | doc |

## Closure criteria

Every check terminal. Findings on:
- any region-specific code path that breaks ADR-0015
- any cross-region drift alert that's silent in practice
