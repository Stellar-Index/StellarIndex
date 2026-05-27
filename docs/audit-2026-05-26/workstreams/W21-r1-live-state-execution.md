# W21 — R1 live state vs claimed state

## Scope

Execute the R1 probe protocol from
[12-r1-live-probe-protocol.md](../12-r1-live-probe-protocol.md). Capture
transcripts under [evidence/r1-probes/](../evidence/r1-probes/).

## Method

For each probe R1-P01..R1-P22 in the probe protocol:

1. Run the documented command on r1 via SSH.
2. Capture raw output in `evidence/r1-probes/r1-pNN-<YYYYMMDD>.md`.
3. State the claim being tested.
4. Compare output against the claim.
5. If discrepancy, file a finding.

## Special focus

- **F-0001 seed**: r1 root partition (`/dev/md1`) is 100% full
  at audit-start (2026-05-26 23:14 UTC). Probe R1-P03 captures
  the state; subsequent investigation must determine what's
  consuming root.
- **W34 cross-ref**: probe R1-P15 captures verify-archive
  state.
- **W27 cross-ref**: probe R1-P14, R1-P21 capture soroban_events
  state.
- **W30 cross-ref**: probe shows whether cold tier is enabled
  on r1.

## Closure criteria

At least 22 probe transcripts under `evidence/r1-probes/`. Every
discrepancy filed. R1-P03's root-disk finding has remediation
in place before audit closes.
