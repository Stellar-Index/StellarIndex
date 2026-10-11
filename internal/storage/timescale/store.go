package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/redact"
)

// Store is the handle on our TimescaleDB connection pool.
// Safe for concurrent use.
type Store struct {
	db *sql.DB

	// lockClock times the bounded chunk-lock retry loop; zero is the wall clock.
	lockClock lockClock

	// usdVolumeQuoteSpec, when non-nil, lets [InsertTrade] populate `trades.usd_volume` for
	// on-chain trades whose quote asset is on the operator's USD-pegged list. Nil keeps the
	// off-chain-only behaviour (tests, ops binary).
	usdVolumeQuoteSpec *USDVolumeQuoteSpec

	// usdVolumeFXResolver, when non-nil, is consulted by [InsertTrade] only after
	// [usdVolumeQuoteSpec] declined the trade. It prices the QUOTE asset in USD and multiplies
	// quote_amount; if that also declines (a pure-Soroban SEP-41 quote with no USD market), the
	// same resolver prices XLM and multiplies base_amount (the XLM-base anchor for pools stored
	// as base=XLM, quote=TOKEN). See [tradeUSDVolumeViaFX] and [usdVolumeViaXLMBaseAnchor].
	// Nil leaves only off-chain and operator-allow-listed trades with a non-NULL `usd_volume`.
	usdVolumeFXResolver USDVolumeFXResolver

	// deriveGeneration is stamped into `derive_generation` by the derived-value writers
	// ([InsertTrade], [BatchInsertTrades], [InsertOracleUpdate], [InsertSupply]). They DO UPDATE
	// in place on conflict, guarded by `derive_generation <= EXCLUDED.derive_generation`, so a
	// corrected re-derive lands without a destructive DELETE + re-backfill. 0 is live ingest; a
	// re-derive entry point sets a POSITIVE value so a later live gen-0 replay can never revert it.
	deriveGeneration int64

	// usdVolumeResolutionInstalled records that InstallUSDVolumeResolution was CALLED, even if
	// it wired no resolvers (a no-pegs deployment). [Store.reDeriveNullVolumeGuard] uses it to
	// fail trade writes closed when a re-derive entry point forgets resolution mode.
	usdVolumeResolutionInstalled bool

	// assetRegistryDedupe caches asset_id -> last successful classic_assets upsert time; a repeat
	// trade inside `assetRegistryDedupeTTL` skips the round-trip so `last_seen_*` and
	// `observation_count` still advance on later ones. Per-Store, not per-process: asset_id is
	// only unique within one database, so a shared map would let a second Store over another DB
	// inherit "already upserted" for rows that DB never received.
	assetRegistryDedupe sync.Map // key: asset_id (string) → time.Time (last upsert)

	// assetRegistryMinLedger is the lowest first ledger upserted per asset
	// (asset_id → uint32); a lower one bypasses the TTL above.
	assetRegistryMinLedger sync.Map

	// issuerRegistryDedupe holds issuer G-strkeys already INSERTed into `issuers`. No TTL: the
	// table has no `last_seen` column to advance after the first INSERT.
	issuerRegistryDedupe sync.Map // key: g_strkey (string) → struct{}

	// classicWatermark memoizes the watermark read by [Store.MinClassicComponentLedger]. It belongs
	// to the four OBSERVERS, not to an asset, so it is identical for every asset in a refresh tick;
	// recomputing per asset repeats four MAX(ledger) scans across every hypertable chunk. The TTL
	// ([classicWatermarkTTL]) is far shorter than the 1000-ledger staleness threshold it feeds, so
	// the cache cannot hide a stall.
	classicWatermark classicWatermarkCache

	// sep41Watermark is the [classicWatermark] counterpart for the SEP-41 producer. It is a
	// separate cache because the producers advance independently; one stalling must not be hidden
	// behind the other's progress.
	sep41Watermark classicWatermarkCache
}

// SetUSDVolumeQuoteSpec installs the quote-asset spec [InsertTrade] uses for on-chain
// `trades.usd_volume`; nil reverts to off-chain-only. Call once at startup, not concurrently
// with InsertTrade.
func (s *Store) SetUSDVolumeQuoteSpec(spec *USDVolumeQuoteSpec) {
	s.usdVolumeQuoteSpec = spec
}

// SetUSDVolumeFXResolver installs the FX-resolver tier for on-chain USD volume; nil clears it. It
// is consulted only when the quote-spec tier declines (see [tradeUSDVolume]). Call once at
// startup, not concurrently with InsertTrade.
func (s *Store) SetUSDVolumeFXResolver(r USDVolumeFXResolver) {
	s.usdVolumeFXResolver = r
}

// SetDeriveGeneration sets the generation the derived-value writers stamp into `derive_generation`.
//
// A re-derive tool passes a POSITIVE value so its rows win the ON CONFLICT guard
// (`derive_generation <= EXCLUDED.derive_generation`); time.Now().Unix() is positive and monotonic,
// so a later re-derive supersedes an earlier one. Call once at startup, not concurrently with writers.
func (s *Store) SetDeriveGeneration(gen int64) {
	s.deriveGeneration = gen
}

// reDeriveNullVolumeGuard fails a trade write CLOSED in re-derive mode (deriveGeneration > 0) when
// the USD-volume resolvers were never installed. A positive generation wins the ON CONFLICT guard
// and assigns `usd_volume = EXCLUDED.usd_volume`; with no resolver, tradeUSDVolume returns nil for
// on-chain DEX / FX-priced trades, overwriting a correct value with NULL that a live gen-0 replay
// can never restore. It fires only when computed == nil and resolution was never wired: tier 1
// prices USD-quoted CEX trades without a resolver, and an unpriceable trade in a wired re-derive
// legitimately writes NULL. Live ingest (generation 0) is unaffected.
func (s *Store) reDeriveNullVolumeGuard(t canonical.Trade, computed *string) error {
	if s.deriveGeneration > 0 && computed == nil && !s.usdVolumeResolutionInstalled {
		return fmt.Errorf("timescale: refusing to write trade %s/%s (source=%s ledger=%d) "+
			"with NULL usd_volume in re-derive mode (generation=%d) without USD-volume "+
			"resolution installed — the write would overwrite a correct stored value with "+
			"NULL and the generation guard would make it unrecoverable; call "+
			"InstallUSDVolumeResolution before writing trades (A-CRIT-1 fail-closed guard)",
			t.Pair.Base, t.Pair.Quote, t.Source, t.Ledger, s.deriveGeneration)
	}
	return nil
}

// resolveUSDVolume is the one place a trade write computes its usd_volume. A resolver read error
// stores NULL on the live path (a later re-derive can repair it) but fails the write in re-derive
// mode, where the NULL would win the generation guard over a correct stored value.
func (s *Store) resolveUSDVolume(ctx context.Context, t canonical.Trade) (*string, error) {
	v, err := tradeUSDVolumeChecked(ctx, t, s.usdVolumeQuoteSpec, s.usdVolumeFXResolver)
	if err != nil {
		if s.deriveGeneration > 0 {
			return nil, fmt.Errorf("timescale: refusing to write trade %s/%s (source=%s ledger=%d) "+
				"in re-derive mode (generation=%d): USD-volume resolver read failed: %w",
				t.Pair.Base, t.Pair.Quote, t.Source, t.Ledger, s.deriveGeneration, err)
		}
		return nil, nil
	}
	if err := s.reDeriveNullVolumeGuard(t, v); err != nil {
		return nil, err
	}
	return v, nil
}

// Pool-tuning constants, exported so store_test.go can assert configurePool set them.
const (
	// PoolMaxOpenConns caps total conns held by one indexer/api/aggregator binary.
	PoolMaxOpenConns = 25
	// PoolMaxIdleConns keeps a small idle floor to avoid a connect storm on a cold cache.
	PoolMaxIdleConns = 5
	// PoolConnMaxLifetime retires every conn this often regardless of liveness, so dead conns
	// cannot linger after a postgres crash and recovery. 30 min beats a rolling-restart interval
	// and bounds the longest cascade-gap to it.
	PoolConnMaxLifetime = 30 * time.Minute
	// PoolConnMaxIdleTime bounds how long an idle conn the DB side already killed (terminate,
	// firewall timeout, failover) can be handed out, which would force a serve-time retry.
	PoolConnMaxIdleTime = 5 * time.Minute
)

// configurePool applies the standard pool tunings to a freshly opened *sql.DB. It is extracted so
// store_test.go can verify the settings without a real postgres.
func configurePool(db *sql.DB) {
	db.SetMaxOpenConns(PoolMaxOpenConns)
	db.SetMaxIdleConns(PoolMaxIdleConns)
	db.SetConnMaxLifetime(PoolConnMaxLifetime)
	db.SetConnMaxIdleTime(PoolConnMaxIdleTime)
}

// Open initialises a connection pool and pings it so a bad DSN fails fast. Pool tuning comes from
// [configurePool].
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := sessionConnConfig(dsn)
	if err != nil {
		return nil, err
	}
	db := stdlib.OpenDB(*cfg)
	configurePool(db)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// sessionConnConfig parses dsn for every pool and pins the session TimeZone to UTC, overriding the
// server default, PGTZ and any DSN setting. Calendar interval arithmetic on a timestamptz
// (`now() - INTERVAL '1 day'`) runs in the session zone, so the sargable closed-bucket guards
// only match the UTC time_bucket grid when the session is UTC; a DST transition shifts them an hour.
func sessionConnConfig(dsn string) (*pgx.ConnConfig, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, dsnParseError(dsn, err)
	}
	// GUC names are case-insensitive: drop every spelling so exactly one
	// timezone reaches the startup packet.
	for k := range cfg.RuntimeParams {
		if strings.EqualFold(k, "timezone") {
			delete(cfg.RuntimeParams, k)
		}
	}
	cfg.RuntimeParams["timezone"] = "UTC"
	return cfg, nil
}

// dsnParseError renders pgx's rejection of dsn without leaking the password. pgx's own text uses a
// redactor that stops at the first `/` or `@`, and the error is left unwrapped so no caller can
// reach ParseConfigError.ConnString. The branch mirrors pgx's URL-prefix test: anything else is
// keyword/value, whose syntax errors quote a password's tail as a key, so none of pgx's text is
// kept. A URL's inner reason is kept only when it parses, since only then it is about an option.
func dsnParseError(dsn string, err error) error {
	const prefix = "timescale: pgx.ParseConfig: "
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return errors.New(prefix + keywordDSNFailure(err))
	}
	if _, uerr := url.Parse(dsn); uerr != nil {
		return errors.New(prefix + redact.ParseFailure(dsn, uerr))
	}
	msg := prefix + redact.ParseFailure(dsn, err)
	if inner := errors.Unwrap(err); inner != nil {
		msg += " (" + redact.Known(inner.Error(), dsn) + ")"
	}
	return errors.New(msg)
}

// keywordDSNKinds maps a phrase of pgx's rejection to our description, most specific first. The
// phrase is matched, never printed, so a password holding one misnames the failure and leaks nothing.
var keywordDSNKinds = []struct{ phrase, kind string }{
	{"failed to parse as keyword/value", `a syntax error (quote a value holding a space or quote as '...', escaping \ and ')`},
	{"not in ConnStringAllowedKeys", "an unrecognised keyword"},
	{"failed to read service", "an unreadable service file"},
	{"invalid connect_timeout", "an invalid connect_timeout"},
	{"port numbers to", "a port count that does not match the host count"},
	{"invalid port", "an invalid port"},
	{"sslmode is invalid", "an invalid sslmode"},
	{"failed to configure TLS", "an invalid TLS setting (sslmode, sslrootcert, sslcert, sslkey)"},
	{"target_session_attrs", "an invalid target_session_attrs"},
	{"protocol_version", "an invalid min_protocol_version or max_protocol_version"},
	{"channel_binding", "an invalid channel_binding"},
	{"require_auth", "an invalid require_auth"},
}

// keywordDSNFailure names what pgx rejected in a keyword/value DSN.
func keywordDSNFailure(err error) string {
	msg := err.Error()
	for _, k := range keywordDSNKinds {
		if strings.Contains(msg, k.phrase) {
			return k.kind + " in the keyword/value connection string"
		}
	}
	return "an invalid keyword/value connection string"
}

// PingContext exercises the pool for the indexer's resilience probe. It is an observability hook;
// reconnection is database/sql plus ConnMaxLifetime. A nil receiver returns nil so shutdown and
// test teardown can poll an unwired Store.
func (s *Store) PingContext(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
}

// Close releases the connection pool. Safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB for packages that need raw
// access (e.g. the migrate binary). Prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// DeriveGeneration returns the generation the derived-value writers stamp (see
// [SetDeriveGeneration]); 0 is live ingest. Ops entry points assert it is positive before writing.
func (s *Store) DeriveGeneration() int64 { return s.deriveGeneration }

// ─── Error helpers ─────────────────────────────────────────────────

// ErrNotFound indicates a row we expected to exist did not.
var ErrNotFound = errors.New("timescale: not found")
