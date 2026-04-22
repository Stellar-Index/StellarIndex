# stellar-archivist (Go, archived) & rs-stellar-archivist (Rust, active)

**Status:** ✅ Use **`rs-stellar-archivist`** (Rust). The Go version is
frozen in the archived `stellar/go` monorepo and will not get fixes.

## Why we care

To run a self-hosted archival core (or Galexie, which embeds captive-core),
we need a local Stellar **history archive**. The fastest path to one is
to mirror an existing public archive (SDF's, LOBSTR's, etc.) onto our
storage. `stellar-archivist` does that.

If we ever want to publish our own archive, this is also the tool that
scans and validates it.

## Version landscape

| Tool                              | Language | Status (2026-04-22) | Notes                                                                                          |
| --------------------------------- | -------- | ------------------- | ---------------------------------------------------------------------------------------------- |
| `stellar/go/tools/stellar-archivist` | Go       | ❌ Frozen — `stellar/go` **archived 2025-12-16** | Still works, but no patches. Documented here only because legacy SDF docs still link to it. |
| `stellar/rs-stellar-archivist`    | Rust     | 🧪 Active dev, v0.1.0, no tagged release yet | Last commit 2026-04-20. This is the path forward.                                              |

## Rust version — verified from source

Verified against: `src/cli/mod.rs`, `src/cli/mirror.rs`, `src/cli/scan.rs`,
`src/storage.rs`, `Cargo.toml` at clone time (2026-04-22).

### Subcommands (code-verified)

Only **two** — enforced at the CLI enum level (`src/cli/mod.rs:104-109`):

- `mirror` — copy files from source → destination.
- `scan`   — verify integrity without writing.

**No `repair`, `dumpxdr`, `status` in the Rust port.** The user's
operational tip ("use `mirror` instead of `repair`, + `-c`") becomes
trivial — `repair` isn't an option here anyway.

### Global flags (verified, `src/cli/mod.rs:46-101`)

```
-c, --concurrency          N   (default 32)   # checkpoints processed concurrently
    --skip-optional             # skip optional SCP files
    --debug
    --trace
    --max-retries          N   (default 3)
    --retry-min-delay-ms   N   (default 100)
    --retry-max-delay-secs N   (default 30)
    --max-concurrent       N   (default 64)   # per-backend I/O concurrency
    --timeout-secs         N   (default 30)
    --io-timeout-secs      N   (default 300)
    --bandwidth-limit      N   (default 0 = unlimited)   # bytes/sec
    --atomic-file-writes        # temp-file + fsync + rename (slower, safer)
    --verify                    # SHA-256 of bucket files (decompress+hash)
```

### `mirror` flags (`src/cli/mirror.rs:11-34`)

```
mirror SRC DST [--low N] [--high N] [--overwrite] [--allow-mirror-gaps]
```

The `SRC` accepts any supported URL scheme. The `DST`'s docstring and
code path say it **must be `file://`** — confirmed by
`OpendalStore::filesystem()` being the only constructor that sets
`writable: true` (`src/storage.rs:353-360`). All other backends pass
`writable: false` and their `open_writer` returns
`"Write not supported by this backend"`.

So: **mirror reads cloud → writes filesystem**. To put the mirror in S3
you mirror to local then `aws s3 sync` separately.

### Storage backends (verified from `Cargo.toml` + `src/storage.rs`)

Built on Apache OpenDAL. Cargo `default-features` = `["cli",
"opendal-all"]` which enables:

| URL scheme(s)                  | Read | Write | Feature flag        | Env vars used when URL has no explicit config                                         |
| ------------------------------ | ---- | ----- | ------------------- | ------------------------------------------------------------------------------------- |
| `file://`                      | ✅   | ✅    | always              | —                                                                                     |
| `http://` / `https://`         | ✅   | ❌    | always              | —                                                                                     |
| `s3://bucket/prefix`           | ✅   | ❌    | `opendal-s3`        | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION`, `S3_ENDPOINT`            |
| `gcs://bucket/prefix` / `gs://` | ✅   | ❌    | `opendal-gcs`       | `GOOGLE_APPLICATION_CREDENTIALS`                                                      |
| `azblob://container/prefix` / `azure://` | ✅ | ❌    | `opendal-azblob`    | `AZURE_STORAGE_ACCOUNT`, `AZURE_STORAGE_KEY`, `AZURE_STORAGE_ENDPOINT`               |
| `b2://bucket/prefix`           | ✅   | ❌    | `opendal-b2`        | `B2_BUCKET_ID`, `B2_APPLICATION_KEY_ID`, `B2_APPLICATION_KEY`                         |
| `swift://container/prefix`     | ✅   | ❌    | `opendal-swift`     | `SWIFT_ENDPOINT`, `SWIFT_TOKEN`                                                       |
| `sftp://[user@]host[:port]/path` | ✅ | ❌    | `opendal-sftp` (unix-only) | `SFTP_USER`, `SFTP_KEY`                                                               |

The README's claim that "it supports both HTTP/HTTPS and filesystem
sources" is **out of date** — the code supports far more sources
out-of-the-box.

### Retry / reliability

- Retries are handled at the **pipeline level**, not in the OpenDAL
  retry layer — explicitly to avoid partial writes during streaming
  (`src/storage.rs:234-244`).
- Error classification distinguishes `Retry` / `Fatal` / `NotFound`
  (`src/storage.rs:21-62`).
- HTTP 4xx are fatal; 5xx, 408, 429 are retryable.
- Empty (0-byte) objects are treated as non-existent for existence
  checks (`src/storage.rs:672-679`) — protects us from half-uploaded
  objects in a source archive.

### What the Rust port is *missing* vs Go

Flags not present:

- `--last N` (act on last N ledgers)
- `-r / --recent` (delta between two archives)
- `--dryrun / -n`
- `--force / -f`
- `--thorough` (decode+re-encode all buckets)
- `--s3region`, `--s3endpoint` — S3 is now configured by env or the
  `s3://` URL path.

For our use case (`mirror` from SDF pubnet to local FS), none of these
are needed.

## `mirror` vs `repair` — user tip (captured for posterity)

> User (2026-04-22): "Still checking around, but using `mirror` instead
> of `repair` is, I believe, faster for ingestion (and can be sped up
> more with `-c`)."

In the **Go** tool:

- `mirror` copies every file in range — no scan phase; fast on an empty
  destination, wasteful on a full one.
- `repair` scans the destination first, then only copies missing files —
  overhead-heavy on an empty destination, optimal on a near-complete one.

The user is right: for a fresh seed, `mirror -c <higher>` wins. In the
Rust port this is moot (no `repair`), so we just use `mirror`.

The `--overwrite` flag in Rust (not in Go) makes re-mirror over an
existing FS mirror safe without manual pre-cleanup.

## Operational plan

### Initial seed (pubnet, one-shot)

```bash
export RUST_LOG=info
stellar-archivist mirror \
  https://history.stellar.org/prd/core-live/core_live_001/ \
  file:///mnt/stellar/history/ \
  --concurrency 64
```

Back it up against multiple sources for redundancy:

```bash
# LOBSTR archive as a secondary mirror
stellar-archivist mirror \
  https://stellar-history.prd.stellar.lobstr.co/ \
  file:///mnt/stellar/history-mirror-2/ \
  --concurrency 64
```

### Incremental top-up (cron, hourly)

```bash
# Work out latest ledger we already have, then mirror from there.
LAST_CKPT=$(/opt/scripts/last-checkpoint.sh /mnt/stellar/history)
stellar-archivist mirror \
  https://history.stellar.org/prd/core-live/core_live_001/ \
  file:///mnt/stellar/history/ \
  --low $LAST_CKPT --concurrency 32
```

### Republish to our own S3/MinIO (two-step)

Because the Rust port can't write to cloud targets, we split:

```bash
rs-stellar-archivist mirror $SRC file:///mnt/history/ -c 64
aws s3 sync /mnt/history/ s3://ctx-rates-history/ \
  --endpoint-url https://minio.colo.ctx.io \
  --delete
```

## Open items

- [ ] Benchmark `--concurrency` sweep (32/64/128) from our colo to pin
      the right default. Likely bound by SDF archive-host rate limits
      rather than our bandwidth.
- [ ] Pin a specific rs-stellar-archivist commit SHA in our build system
      (no tagged release yet).
- [ ] Decide whether we self-host a public-facing archive too (cost:
      egress bandwidth per SDF's own guidance). Likely **no** for Phase 1.
- [ ] Confirm `--verify` cost on a full pubnet archive — worth running
      once per seed, but probably not continuously.

## References

- Repo: <https://github.com/stellar/rs-stellar-archivist>
- OpenDAL: <https://opendal.apache.org>
- SDF publishing guide (notes only `scan`/`repair`, pre-dates Rust port):
  <https://developers.stellar.org/docs/validators/admin-guide/publishing-history-archives>
