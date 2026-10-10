package clickhouse

// Regression tests: an account page came back SHORT while older
// history remained, and the handlers emit `next_cursor` only on a FULL page
// (internal/api/v1/explorer/accounts.go — the OpenAPI contract says the cursor
// is "absent on the last page"), so a client walking the history stopped there
// with older transactions unreached. Silent truncation, not a cosmetic short
// page.
//
// Cause: the keyset merge of the sourced and participant arms took its limit
// over keys that still held cross-arm DUPLICATES. A tx the account sourced
// that ALSO carries it as a non-source participant of one of its operations is
// emitted by BOTH arms, so each such tx consumed two slots and the page lost a
// row per overlap. The merge (mergeKeysDesc) must dedupe BEFORE its cut, so
// the keyset is exactly min(limit, distinct keys older than the cursor).
