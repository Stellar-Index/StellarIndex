package clickhouse

// Regression tests: an account page came back short while older history remained, and handlers emit
// `next_cursor` only on a full page, so clients stopped early. The keyset merge cut its limit over cross-arm
// duplicate keys; mergeKeysDesc must dedupe before its cut.
