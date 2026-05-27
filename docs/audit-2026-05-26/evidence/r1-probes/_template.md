# R1 Probe Transcript Template

Copy this file to `r1-pNN-YYYYMMDD.md` (e.g.
`r1-p03-2026-05-26.md`) and fill in.

---

# R1-PNN — <topic>

## Subject

What this probe tests. One sentence.

## Claim being tested

The factual claim. State precisely what would make this claim
true vs false.

## When

`YYYY-MM-DDTHH:MM:SSZ` (UTC).

## Where

Host: `root@136.243.90.96` (R1).

## Command(s)

```sh
<exact command, copy-pasteable>
```

## Output

```
<raw output, truncated to relevant lines only>
```

## Interpretation

What does the output say? Does it confirm or contradict the
claim?

## Disposition

- `claim-confirmed`
- `claim-contradicted` (file finding)
- `inconclusive` (note what's missing, schedule a re-probe)

If `claim-contradicted`, link the finding ID: `F-####`.
