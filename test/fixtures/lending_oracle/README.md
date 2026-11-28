# Lending pool oracle fixtures

Real mainnet `contract_data` ledger entries of the five oracle-aggregator adapters known to sit behind Blend /
BLNT pools: four Blend Capital builds and one YieldBlox fork. They are the adapters found so far, not a census of
every pool's oracle. Pinned by `internal/sources/blend/oracleagg/config_real_test.go` and
`recipe_ybfork_test.go`, which decode them to the configurations recorded in
[docs/operations/wasm-audits/oracle-aggregator.md](../../../docs/operations/wasm-audits/oracle-aggregator.md).

| file | sha256 | rows |
| --- | --- | --- |
| `instances.tsv` | `370842c809437f7c0bdb5ffbf22f890992343b83eda0e59e789410a01e112871` | lines 1-4: `key_xdr`, `last_modified_ledger`, `entry_xdr` of each adapter's instance entry; lines 5-8: `<adapter prefix>`, entry count, ledger (a capture summary, not entries) |
| `v1-entries.tsv` | `8cc2684ce3b4dcf6985211d39b323c64438d7bd313f011611913848de545084e` | `<adapter prefix>`, `key_xdr`, `last_modified_ledger`, `entry_xdr`: all seven `contract_data` entries of each v1 adapter |
| `reflector-instances.tsv` | `11f028b726ad72eadd9de5f016a5c4391c6e1041df4791a0ec5390c2f7226e50` | `CEX` / `DEX`, `key_xdr`, `ledger_seq`, `entry_xdr`: the instance entry of each Reflector feed (`CAFJZQWS…`, `CALI2BYU…`) at L65,712,333 / L65,712,329; `CEX-count` / `DEX-count` rows: each contract's other `contract_data` entry count (a capture summary, not entries). Pinned by `feedstate_real_test.go` |
| `ybfork-instance.tsv` | `1ad2bd1474e869eaf0dde5b15d7b9b9afe237b9e56dabad640847ff7c5419473` | one line: `key_xdr`, `last_modified_ledger`, `entry_xdr` of `CD74A3C5`'s instance entry (L59,817,248, its current state) |

| adapter | WASM | program |
| --- | --- | --- |
| `CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS` | `41df0489…0bfd` | v2.0.0 |
| `CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36` | `41df0489…0bfd` | v2.0.0 |
| `CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y` | `a7463634…c6bb` | v1.0.0 |
| `CCPXGHMVZROX7G6OIP4VBN7FJO6KJAMASNQXLISC3QI52EQVX3VKIWO6` | `a7463634…c6bb` | v1.0.0 |
| `CD74A3C54EKUVEGUC6WNTUPOTHB624WFKXN3IYTFJGX3EHXDXHCYMXXR` | `8cf43882…cf76` | YieldBlox fork v2.0.0 |

Captured for the adapter audit on 2026-11-28; instances are at their last-modified ledgers L55,857,810 to
L65,319,857. Copy, do not edit: the bytes are the evidence.
