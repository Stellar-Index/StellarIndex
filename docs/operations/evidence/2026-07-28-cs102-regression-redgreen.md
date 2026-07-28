---
title: Evidence — CS-102 supply-freshness anchor regression tests (red/green)
last_verified: 2026-07-28
status: final
---

# CS-102 regression tests — red/green proof, 2026-07-28 ~20:20Z

**Claim:** the two integration tests written for the CS-102 fix family
actually guard the defect (frozen supply via wrong freshness anchor) —
they fail against the defective code and pass against the fix. A test
that has only ever been seen green proves nothing about its trigger.

**Tests** (build tag `integration`, real TimescaleDB 2.26.4-pg15 via
testcontainers-go):
- `test/integration/classic_supply_storage_test.go` →
  `TestMinClassicComponentLedgerUsesObserverWatermark`
- `test/integration/sep41_supply_storage_test.go` →
  `TestMinSEP41ComponentLedgerUsesObserverWatermark`

**RED — defect deliberately re-introduced** (per-asset / per-contract
last-activity anchors restored in
`internal/storage/timescale/{classic_supply_observations,sep41_supply_events}.go`,
marked `PROBE(temporary)`):

```
classic_supply_storage_test.go:377: quiet asset anchor = 1000, want 5000
  (the observer watermark). Got 1000 => the query regressed to per-asset
  last-activity, which freezes any asset without recent claimable activity.
--- FAIL: TestMinClassicComponentLedgerUsesObserverWatermark (3.08s)

sep41_supply_storage_test.go:989: quiet contract anchor = 1000, want 9000
  (producer watermark). Got 1000 => regressed to per-contract last
  activity, which freezes every SEP-41 token whose supply has not
  recently changed.
--- FAIL: TestMinSEP41ComponentLedgerUsesObserverWatermark (2.57s)
```

**GREEN — fix restored** (`git checkout` of the two files, tree clean):

```
go test -tags integration -run 'TestMinClassicComponentLedgerUsesObserverWatermark|TestMinSEP41ComponentLedgerUsesObserverWatermark' ./test/integration/ -count=1
ok  github.com/Stellar-Index/StellarIndex/test/integration  6.637s
```

**What this does NOT prove:** that the fixes behave on r1 (they are
undeployed until v0.21.2), nor anything about the sep41 projector
restart / tail rebuild, which is a separate producer-side fix
(`ae7a082d`) with its own post-deploy acceptance (watermark advancing +
`/v1/coverage` complete).
