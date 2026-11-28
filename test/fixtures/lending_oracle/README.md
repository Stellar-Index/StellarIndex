# Lending pool oracle fixtures

Real mainnet `contract_data` ledger entries of the four Blend Capital `oracle-aggregator` adapters that
Blend / BLNT pools point at. Pinned by `internal/sources/blend/oracleagg/config_real_test.go`, which decodes
them to the configurations recorded in
[docs/operations/wasm-audits/oracle-aggregator.md](../../../docs/operations/wasm-audits/oracle-aggregator.md).

| file | sha256 | rows |
| --- | --- | --- |
| `instances.tsv` | `370842c809437f7c0bdb5ffbf22f890992343b83eda0e59e789410a01e112871` | lines 1-4: `key_xdr`, `last_modified_ledger`, `entry_xdr` of each adapter's instance entry; lines 5-8: `<adapter prefix>`, entry count, ledger (a capture summary, not entries) |
| `v1-entries.tsv` | `8cc2684ce3b4dcf6985211d39b323c64438d7bd313f011611913848de545084e` | `<adapter prefix>`, `key_xdr`, `last_modified_ledger`, `entry_xdr`: all seven `contract_data` entries of each v1 adapter |

| adapter | WASM | program |
| --- | --- | --- |
| `CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS` | `41df0489…0bfd` | v2.0.0 |
| `CDN7JD6SP32RZHB3DHK4S4AWCCQF6GYIUO455UCR4RGRZILZDRD24N36` | `41df0489…0bfd` | v2.0.0 |
| `CATKK5ZNJCKQQWTUWIUFZMY6V6MOQUGSTFSXMNQZHVJHYF7GVV36FB3Y` | `a7463634…c6bb` | v1.0.0 |
| `CCPXGHMVZROX7G6OIP4VBN7FJO6KJAMASNQXLISC3QI52EQVX3VKIWO6` | `a7463634…c6bb` | v1.0.0 |

Captured for the adapter audit on 2026-11-28; instances are at their last-modified ledgers L55,857,810 to
L65,319,857. Copy, do not edit: the bytes are the evidence.
