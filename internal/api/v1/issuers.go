package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// IssuersReader is the seam the issuer handlers read through.
// timescale.Store satisfies it via GetIssuer + ListIssuerAssets +
// ListIssuers.
type IssuersReader interface {
	GetIssuer(ctx context.Context, gStrkey string) (timescale.IssuerRow, error)
	ListIssuerAssets(ctx context.Context, gStrkey string) ([]timescale.IssuerAsset, error)
	ListIssuers(ctx context.Context, limit int) ([]timescale.IssuerSummary, error)
}

// issuersAtReader is what the TTL cache adds to IssuersReader: the list
// read also returns the served entry's fill time, stamped as as_of.
type issuersAtReader interface {
	ListIssuersAt(ctx context.Context, limit int) ([]timescale.IssuerSummary, time.Time, error)
}

// IssuerListEntry is the wire shape of one row in /v1/issuers.
// Compact summary suitable for the issuer-directory page.
//
// OrgName is the issuer's organisation name from SEP-1
// (`[DOCUMENTATION].ORG_NAME` in stellar.toml). Populated by
// the `stellarindex-ops sep1-refresh` job; empty when never
// resolved or when the issuer has no documentation block.
type IssuerListEntry struct {
	GStrkey    string `json:"g_strkey"`
	HomeDomain string `json:"home_domain,omitempty"`
	OrgName    string `json:"org_name,omitempty"`
	// OrgVerified is true only when the issuer's SEP-1 toml lists it back
	// (bidirectional). Group/merge issuers by org only when true.
	OrgVerified           bool  `json:"org_verified"`
	AssetCount            int64 `json:"asset_count"`
	TotalObservationCount int64 `json:"total_observation_count"`
	// ScamReason is non-empty when the issuer is flagged as scam /
	// malicious by the curated `known_scams.go` map (sourced from
	// stellar.expert's directory). Clients should render a warning
	// badge — this issuer's assets shouldn't be trusted.
	ScamReason string `json:"scam_reason,omitempty"`
}

// Issuer is the wire shape returned by /v1/issuers/{g_strkey}.
type Issuer struct {
	GStrkey    string `json:"g_strkey"`
	HomeDomain string `json:"home_domain,omitempty"`
	// OrgName is the issuer's organisation name extracted from
	// SEP-1 (`[DOCUMENTATION].ORG_NAME`). Same field as the
	// listing endpoint surfaces; populated by the
	// `stellarindex-ops sep1-refresh` job.
	OrgName string `json:"org_name,omitempty"`
	// OrgVerified is true only when the issuer's SEP-1 toml lists it back
	// (bidirectional proof — one-way is spoofable). When false, OrgName is
	// issuer-self-declared; clients MUST NOT render it as authoritative,
	// or it becomes an impersonation vector. Always present so the
	// absence of the field can't be mistaken for "verified".
	OrgVerified bool `json:"org_verified"`
	// ScamReason is non-empty when the issuer is flagged as scam /
	// malicious by the curated `known_scams.go` map (sourced from
	// stellar.expert's directory).
	ScamReason    string `json:"scam_reason,omitempty"`
	AuthRequired  *bool  `json:"auth_required,omitempty"`
	AuthRevocable *bool  `json:"auth_revocable,omitempty"`
	AuthImmutable *bool  `json:"auth_immutable,omitempty"`
	AuthClawback  *bool  `json:"auth_clawback,omitempty"`
	// AuthFlagsSource says how the four auth_* flags above were obtained.
	// `live` — decoded from the account's CURRENT on-chain
	// AccountEntry. `last_known_before_removal` — the issuer has MERGED ITS
	// ACCOUNT AWAY and these are its flags as of `auth_flags_as_of_ledger`;
	// they are a historical record, NOT the issuer's current authorisation
	// policy, and must not be rendered as one. ~10.2k of ~59.2k known
	// issuers are in that state. Omitted when the provenance is unknown
	// (flags not resolved, or persisted before migration 0153 and not yet
	// re-drained) — absence means "unknown", never "current".
	AuthFlagsSource string `json:"auth_flags_source,omitempty"`
	// AuthFlagsAsOfLedger is the ledger the flags are true as of. Omitted
	// when unknown.
	AuthFlagsAsOfLedger *uint32         `json:"auth_flags_as_of_ledger,omitempty"`
	SEP1ResolvedAt      *string         `json:"sep1_resolved_at,omitempty"`
	SEP1Payload         json.RawMessage `json:"sep1_payload,omitempty"`
	CreationLedger      *uint32         `json:"creation_ledger,omitempty"`
	Assets              []IssuedAsset   `json:"assets,omitempty"`
	// CoverageNote is an honest-degrade signal (mirrors ProtocolsView /
	// AccountMovements): non-empty when ListIssuerAssets failed (including
	// a deadline) and Assets was omitted rather than fabricated as a
	// genuine zero-asset issuer. Absent = Assets is complete (or the issuer
	// genuinely issues nothing).
	CoverageNote string `json:"coverage_note,omitempty"`
}

// IssuedAsset is one entry in the issuer's `assets` list.
type IssuedAsset struct {
	AssetID          string `json:"asset_id"`
	Code             string `json:"code"`
	Slug             string `json:"slug"`
	FirstSeenLedger  uint32 `json:"first_seen_ledger"`
	LastSeenLedger   uint32 `json:"last_seen_ledger"`
	ObservationCount int64  `json:"observation_count"`
}

// IssuersListDefaultLimit is the limit handleIssuersList applies when
// the request omits `?limit=`. Exported so the prewarm — which must
// keep this exact key warm — references the one source of truth
// instead of a second, driftable copy.
const IssuersListDefaultLimit = 100

// IssuersListMaxLimit is the largest `?limit=` handleIssuersList accepts.
const IssuersListMaxLimit = 500

// handleIssuersList serves GET /v1/issuers.
//
// Returns the issuer directory ordered by total observation count
// across the issuer's classic assets — the proxy-for-activity
// ranking the explorer /issuers page exposes. Returns 503 when
// no IssuersReader is wired and 400 on out-of-range limit.
func (s *Server) handleIssuersList(w http.ResponseWriter, r *http.Request) {
	if s.Issuers == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuers-unavailable",
			"Issuers unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the issuer reader yet.")
		return
	}
	limit := IssuersListDefaultLimit
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > IssuersListMaxLimit {
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/invalid-limit",
				"Invalid limit", http.StatusBadRequest,
				"limit must be 1-500")
			return
		}
		limit = n
	}
	// 8s ceiling — same pattern as the cold-path series.
	listCtx, listCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer listCancel()
	var vintage dataVintage
	var rows []timescale.IssuerSummary
	var err error
	if at, ok := s.Issuers.(issuersAtReader); ok {
		var filled time.Time
		rows, filled, err = at.ListIssuersAt(listCtx, limit)
		vintage.note(filled)
	} else {
		rows, err = s.Issuers.ListIssuers(listCtx, limit)
		vintage.note(time.Time{})
	}
	if err != nil {
		if clientAborted(r, err) {
			// Client went away mid-query — e.g. concurrent callers
			// (the sla-probe, a browser navigating away) that cancel
			// in-flight requests. The driver surfaces the resulting
			// context cancellation as `canceling statement due to
			// user request` (SQLSTATE 57014); that is NOT a server
			// fault. Return quietly: no ERROR log, no 500 (which
			// otherwise pollutes the 5xx rate + SLA availability).
			// Same canonical ordering as handleObservations
			// (envelope.go clientAborted doc).
			return
		}
		if handlerTimedOut(listCtx, err) {
			s.logger.Warn("ListIssuers deadline exceeded", "limit", limit)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/issuers-timeout",
				"Issuers list timed out", http.StatusServiceUnavailable,
				"the issuer registry scan didn't return in 8s; retry shortly.")
			return
		}
		if transientStorageErr(err) {
			// Postgres-side cancellation (57014 from statement_timeout
			// / lock_timeout / idle_in_transaction reset), bad-connection
			// after pool retries exhausted, or a network EOF. These
			// are infrastructure transients that a retry would succeed
			// on — surface 503 so sla-probe doesn't book it as a
			// permanent availability failure. Same clientAborted
			// residual as the client-abort branch above.
			s.logger.Warn("issuers list: transient storage error", "err", err)
			writeProblem(w, r,
				"https://api.stellarindex.io/errors/issuers-transient",
				"Issuers list temporarily unavailable", http.StatusServiceUnavailable,
				"the storage layer hit a transient error; retry shortly.")
			return
		}
		s.logger.Warn("issuers list", "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuers-error",
			"Issuers list failed", http.StatusInternalServerError,
			"Storage layer returned an error.")
		return
	}
	out := make([]IssuerListEntry, len(rows))
	for i, r := range rows {
		homeDomain, orgName := enrichIssuer(r.GStrkey, r.HomeDomain, r.OrgName)
		reason := scamReason(r.GStrkey)
		// Identity suppression: a flagged,
		// UNVERIFIED issuer's org_name/home_domain are self-declared
		// on-chain values — for counterfeiters that is the
		// impersonation itself (a "SCAM Counterfeiter" declaring
		// lobstr.co rendered as "LOBSTR — SCAM", indicting the
		// victim brand). Serving the stolen identity as if it were
		// the row's name is disinformation; the G-key + reason are
		// the honest identity for these rows.
		if reason != "" && !r.OrgVerified {
			homeDomain, orgName = "", ""
		}
		out[i] = IssuerListEntry{
			GStrkey:               r.GStrkey,
			HomeDomain:            homeDomain,
			OrgName:               orgName,
			OrgVerified:           r.OrgVerified,
			AssetCount:            r.AssetCount,
			TotalObservationCount: r.TotalObservationCount,
			ScamReason:            reason,
		}
	}
	writeEnvelope(w, Envelope{Data: out, AsOf: vintage.asOf()})
}

// handleIssuer serves GET /v1/issuers/{g_strkey}.
//
// Returns 404 (problem+json) when the issuer has never been observed.
// Always includes the assets array so the explorer issuer card has
// the per-issuer drill-down data without a second request.
func (s *Server) handleIssuer(w http.ResponseWriter, r *http.Request) {
	if s.Issuers == nil {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuers-unavailable",
			"Issuers unavailable", http.StatusServiceUnavailable,
			"This deployment hasn't wired the issuer reader yet.")
		return
	}

	gStrkey := r.PathValue("g_strkey")
	if gStrkey == "" {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/invalid-g-strkey",
			"Invalid G-strkey", http.StatusBadRequest,
			"g_strkey path segment is required")
		return
	}
	// Stellar G-strkeys are uppercase base32 by SEP-23 convention;
	// the storage layer keys off the canonical uppercase form.
	// URL clients (chat clients, search tools, manual typing)
	// regularly lowercase, which would otherwise 404. Normalise
	// at input — base32 alphabet is case-insensitive in Stellar
	// SDK validation, so the underlying ed25519 public key is the
	// same. No risk of merging two distinct accounts.
	gStrkey = strings.ToUpper(gStrkey)

	// 8s ceiling spans both calls — GetIssuer is fast but the
	// fan-out to ListIssuerAssets can hit the trades hypertable
	// for the per-asset observation count.
	iCtx, iCancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer iCancel()
	row, err := s.Issuers.GetIssuer(iCtx, gStrkey)
	if err != nil {
		s.writeIssuerReadProblem(w, r, iCtx, gStrkey, err)
		return
	}

	assets, err := s.Issuers.ListIssuerAssets(iCtx, gStrkey)
	var assetsCoverageNote string
	var assetsReadFailed bool
	if err != nil {
		// Soft-fail on the asset list — the issuer card still
		// renders without it, but the coverage_note distinguishes
		// this from a genuine zero-asset issuer (CA2-A04-harden-9).
		// Includes deadline exceeded.
		s.logger.Warn("issuer assets", "g_strkey", gStrkey, "err", err)
		assetsReadFailed = true
		assets = nil
		assetsCoverageNote = "the asset list for this issuer could not be read " +
			"(storage error or timeout); assets is omitted (not shown as empty) " +
			"and will reappear once the read recovers"
	}

	detailReason := scamReason(row.GStrkey)
	out := Issuer{
		GStrkey:       row.GStrkey,
		HomeDomain:    row.HomeDomain,
		OrgName:       row.OrgName,
		OrgVerified:   row.OrgVerified,
		ScamReason:    detailReason,
		AuthRequired:  row.AuthRequired,
		AuthRevocable: row.AuthRevocable,
		AuthImmutable: row.AuthImmutable,
		AuthClawback:  row.AuthClawback,
		// The persisted provenance stands whenever no live entry resolves.
		AuthFlagsSource:     row.AuthFlagsSource,
		AuthFlagsAsOfLedger: row.AuthFlagsAsOfLedger,
		SEP1ResolvedAt:      row.SEP1ResolvedAt,
		SEP1Payload:         row.SEP1Payload,
		CreationLedger:      row.CreationLedger,
		CoverageNote:        assetsCoverageNote,
	}
	// Identity precedence: DB row → live on-chain account state →
	// curated knownIssuers map. The curated map only fills blanks, so a
	// stale entry cannot beat the account's own signed home_domain.
	s.enrichIssuerFromAccountState(iCtx, gStrkey, &out)
	out.HomeDomain, out.OrgName = enrichIssuer(row.GStrkey, out.HomeDomain, out.OrgName)
	// Identity suppression runs LAST: a flagged, unverified issuer's
	// self-declared identity is the impersonation. Run any earlier, the
	// account-state enrich would refill the cleared home_domain straight
	// from the scammer's own on-chain field. Auth flags stay — they
	// are objective account state, not identity claims.
	//
	// SEP1Payload is suppressed alongside HomeDomain/OrgName: it is the
	// raw stellar.toml JSONB those two fields are extracted FROM, so
	// leaving it populated re-served the same impersonated org_name/
	// home_domain the two assignments above were clearing.
	if detailReason != "" && !out.OrgVerified {
		out.HomeDomain, out.OrgName = "", ""
		out.SEP1Payload = nil
	}
	for _, a := range assets {
		out.Assets = append(out.Assets, IssuedAsset{
			AssetID:          a.AssetID,
			Code:             a.Code,
			Slug:             a.Slug,
			FirstSeenLedger:  a.FirstSeenLedger,
			LastSeenLedger:   a.LastSeenLedger,
			ObservationCount: a.ObservationCount,
		})
	}
	writeJSON(w, out, Flags{Degraded: assetsReadFailed})
}

// writeIssuerReadProblem maps a GetIssuer error to the right problem+json
// response. Split out of handleIssuer to keep it under the funlen ceiling.
func (s *Server) writeIssuerReadProblem(w http.ResponseWriter, r *http.Request, iCtx context.Context, gStrkey string, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuer-not-found",
			"Issuer not found", http.StatusNotFound,
			"This G-strkey hasn't been observed as an issuer.")
		return
	}
	if clientAborted(r, err) {
		// Client went away mid-query — not a server fault.
		// Same canonical ordering as handleIssuersList.
		return
	}
	if handlerTimedOut(iCtx, err) {
		s.logger.Warn("GetIssuer deadline exceeded", "g_strkey", gStrkey)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuer-timeout",
			"Issuer read timed out", http.StatusServiceUnavailable,
			"the issuer + asset list scan didn't return in 8s; retry shortly.")
		return
	}
	if transientStorageErr(err) {
		// Postgres-side cancellation/transient network error — a
		// retry would likely succeed, so surface 503 rather than
		// booking a permanent availability failure. Same pattern
		// as handleIssuersList's ListIssuers path.
		s.logger.Warn("GetIssuer: transient storage error", "g_strkey", gStrkey, "err", err)
		writeProblem(w, r,
			"https://api.stellarindex.io/errors/issuer-transient",
			"Issuer read temporarily unavailable", http.StatusServiceUnavailable,
			"the storage layer hit a transient error; retry shortly.")
		return
	}
	s.logger.Warn("issuer read", "g_strkey", gStrkey, "err", err)
	writeProblem(w, r,
		"https://api.stellarindex.io/errors/issuer-error",
		"Issuer read failed", http.StatusInternalServerError,
		"Storage layer returned an error.")
}

// IssuerAuthFlagsReader is the narrow lake seam /v1/issuers/{g_strkey} reads an
// issuer's current AccountEntry flags + home_domain through.
type IssuerAuthFlagsReader interface {
	BulkAccountAuthFlags(ctx context.Context, gStrkeys []string) (map[string]clickhouse.AccountAuthFlags, error)
}

// enrichIssuerFromAccountState overlays the issuer's live on-chain AccountEntry
// (auth flags, home_domain, as-of ledger) onto the persisted row. Stellar
// AccountEntry flags: AUTH_REQUIRED=1, AUTH_REVOCABLE=2, AUTH_IMMUTABLE=4,
// AUTH_CLAWBACK=8. No-op when no live entry resolves.
//
// A live AccountEntry is the authority on its own account, so it is consulted
// for every row, filled or not: the persisted columns are an older reading of
// the same entry and a filled row is exactly the shape that can hold a domain
// the account has since moved away from. When no live entry resolves, the
// persisted reading stands untouched: absence from the current-state
// projection is what a merged account AND a lake-coverage gap both look like,
// so only the drain, which reads an actual `removed` row, may write
// `last_known_before_removal`.
func (s *Server) enrichIssuerFromAccountState(ctx context.Context, gStrkey string, out *Issuer) {
	live, ok := s.liveIssuerAccount(ctx, gStrkey)
	if !ok {
		return
	}
	req, rev, imm, claw := live.Required, live.Revocable, live.Immutable, live.Clawback
	out.AuthRequired = &req
	out.AuthRevocable = &rev
	out.AuthImmutable = &imm
	out.AuthClawback = &claw
	out.AuthFlagsSource = string(clickhouse.AuthFlagsSourceLive)
	out.AuthFlagsAsOfLedger = nil
	if live.AsOfLedger > 0 {
		asOf := live.AsOfLedger
		out.AuthFlagsAsOfLedger = &asOf
	}
	// An empty live value is NOT a retraction: a merged account's reading is
	// persisted without a domain on purpose, and the lake can lack the field.
	if live.HomeDomain == "" || live.HomeDomain == out.HomeDomain {
		return
	}
	out.HomeDomain = live.HomeDomain
	// The stored SEP-1 identity was fetched from the domain the account no
	// longer declares, so it is not this domain's claim about this account.
	out.OrgName, out.OrgVerified = "", false
	out.SEP1Payload, out.SEP1ResolvedAt = nil, nil
}

// liveIssuerAccount reads the issuer's current AccountEntry: through the
// narrow key_xdr point lookup when wired, else through the explorer's full
// account-state read.
func (s *Server) liveIssuerAccount(ctx context.Context, gStrkey string) (clickhouse.AccountAuthFlags, bool) {
	if s.IssuerAuthFlags != nil {
		m, err := s.IssuerAuthFlags.BulkAccountAuthFlags(ctx, []string{gStrkey})
		if err != nil {
			return clickhouse.AccountAuthFlags{}, false
		}
		f, ok := m[gStrkey]
		return f, ok && f.Source == clickhouse.AuthFlagsSourceLive
	}
	if s.Explorer == nil {
		return clickhouse.AccountAuthFlags{}, false
	}
	st, _, err := s.Explorer.AccountStateCached(ctx, gStrkey)
	if err != nil || !st.Exists {
		return clickhouse.AccountAuthFlags{}, false
	}
	return clickhouse.AccountAuthFlags{
		Required:   st.Flags&0x1 != 0,
		Revocable:  st.Flags&0x2 != 0,
		Immutable:  st.Flags&0x4 != 0,
		Clawback:   st.Flags&0x8 != 0,
		HomeDomain: st.HomeDomain,
		Source:     clickhouse.AuthFlagsSourceLive,
		AsOfLedger: st.LastModifiedLedger,
	}, true
}
