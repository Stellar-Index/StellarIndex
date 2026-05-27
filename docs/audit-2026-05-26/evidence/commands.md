# Command Transcripts

| ID | Date (UTC) | Command | Output excerpt | Workstream | Notes |
| --- | --- | --- | --- | --- | --- |
| CMD-0001 | 2026-05-26 | `git ls-files \| wc -l` | `2104` | W01 | tracked-file count at audit-start |
| CMD-0002 | 2026-05-26 | `git log --oneline v0.5.0-rc.70..HEAD \| wc -l` | `53` | W01 | commit count since baseline |
| CMD-0003 | 2026-05-26 | `ls migrations/*.up.sql \| wc -l` | `45` | W09 | migration count (was 28 at baseline) |
| CMD-0004 | 2026-05-26 | `ls internal/sources/` | accounts aquarius band blend cctp claimable_balances comet defindex external forex frankfurter liquidity_pools phoenix redstone reflector rozo sac_balances sdex sep41_supply sorobanevents soroswap soroswap_router trustlines | W07 | 23 sources (was 18 at baseline + 5 new) |
| CMD-0005 | 2026-05-26 | `ls docs/adr/` | 0001..0029 | W02 | 29 ADRs (was 26) |
| CMD-0006 | 2026-05-26 | `ssh root@136.243.90.96 'df -h'` | `/dev/md1 49G 47G 0 100%` (root partition full) | W21 | seed for F-0001 |

Add entries as commands run.
