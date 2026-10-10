package v1_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubIssuersReader is the in-memory test seam.
type stubIssuersReader struct {
	row     timescale.IssuerRow
	rowErr  error
	assets  []timescale.IssuerAsset
	assetsE error
	list    []timescale.IssuerSummary
	listErr error

	lastGStrkey string
	lastLimit   int
}

func (r *stubIssuersReader) GetIssuer(_ context.Context, gStrkey string) (timescale.IssuerRow, error) {
	r.lastGStrkey = gStrkey
	if r.rowErr != nil {
		return timescale.IssuerRow{}, r.rowErr
	}
	return r.row, nil
}

func (r *stubIssuersReader) ListIssuerAssets(_ context.Context, gStrkey string) ([]timescale.IssuerAsset, error) {
	if r.assetsE != nil {
		return nil, r.assetsE
	}
	return r.assets, nil
}

func (r *stubIssuersReader) ListIssuers(_ context.Context, limit int) ([]timescale.IssuerSummary, error) {
	r.lastLimit = limit
	if r.listErr != nil {
		return nil, r.listErr
	}
	return r.list, nil
}

// ─── /v1/issuers (list) ───────────────────────────────────────────

// TestHandleIssuersList_503WhenReaderNil — feature-gated reader.
func TestHandleIssuersList_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHandleIssuersList_DefaultLimit — no ?limit forwards as 100.
func TestHandleIssuersList_DefaultLimit(t *testing.T) {
	reader := &stubIssuersReader{}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	_ = mustGet(t, ts.URL+"/v1/issuers")
	if reader.lastLimit != 100 {
		t.Errorf("default limit = %d, want 100", reader.lastLimit)
	}
}

// TestHandleIssuersList_InvalidLimit400 — out-of-range / non-numeric
// values return 400 with the `invalid-limit` problem type. Mirrors
// the same guard pattern shipped on /v1/coins, /v1/markets, etc.
func TestHandleIssuersList_InvalidLimit400(t *testing.T) {
	srv := v1.New(v1.Options{Issuers: &stubIssuersReader{}})
	ts := startHTTPTest(t, srv.Handler())

	for _, bad := range []string{"0", "501", "-1", "xyz", "999999"} {
		resp := mustGet(t, ts.URL+"/v1/issuers?limit="+bad)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("limit=%q → %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestHandleIssuersList_HappyPath pins the wire shape — every field
// the explorer's /issuers table reads. ScamReason populates from
// the curated known_scams.go map; not asserted directly here
// (depends on whether the test G-strkey is in the map).
func TestHandleIssuersList_HappyPath(t *testing.T) {
	reader := &stubIssuersReader{
		list: []timescale.IssuerSummary{
			{
				GStrkey:               "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				HomeDomain:            "centre.io",
				OrgName:               "Circle",
				AssetCount:            1,
				TotalObservationCount: 41610623,
			},
			{
				GStrkey:               "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA",
				HomeDomain:            "aqua.network",
				OrgName:               "Aquarius",
				AssetCount:            1,
				TotalObservationCount: 14764050,
			},
		},
	}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers?limit=10")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if reader.lastLimit != 10 {
		t.Errorf("forwarded limit = %d, want 10", reader.lastLimit)
	}
	var env struct {
		Data []v1.IssuerListEntry `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if len(env.Data) != 2 {
		t.Fatalf("len = %d, want 2", len(env.Data))
	}
	first := env.Data[0]
	if first.GStrkey != "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
		t.Errorf("g_strkey drift: %q", first.GStrkey)
	}
	if first.OrgName != "Circle" || first.HomeDomain != "centre.io" {
		t.Errorf("first row metadata = %+v", first)
	}
	if first.AssetCount != 1 || first.TotalObservationCount != 41610623 {
		t.Errorf("counts = (%d, %d)", first.AssetCount, first.TotalObservationCount)
	}
}

// TestHandleIssuersList_TimeoutReturns503 — the 8s deadline fires
// when the issuer registry scan takes too long. Returns 503 with
// `issuers-timeout` problem type.
func TestHandleIssuersList_TimeoutReturns503(t *testing.T) {
	reader := &stubIssuersReader{listErr: context.DeadlineExceeded}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "issuers-timeout") {
		t.Errorf("expected `issuers-timeout` problem type in body, got: %s", body)
	}
}

// TestHandleIssuersList_ReaderError500 — generic storage error.
func TestHandleIssuersList_ReaderError500(t *testing.T) {
	reader := &stubIssuersReader{listErr: errors.New("storage broke")}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

// TestHandleIssuersList_ClientAbortedNo500 — regression for the
// clientAborted guard. When
// the inbound request is canceled mid-flight (concurrent callers /
// the sla-probe / a browser navigating away), the driver surfaces the
// canceled ListIssuers query as SQLSTATE 57014. That is a client
// abort, NOT a server fault: the handler must return quietly, never
// a 500 (a 500 pollutes the 5xx rate + SLA availability — it was the
// sole sla-probe SLA-harness blocker). Without the
// clientAborted guard the canceled-context error falls through to
// the generic `Issuers list failed` 500. clientAborted keys off
// r.Context().Err(), so any storage error + a canceled request ctx
// must NOT 500.
func TestHandleIssuersList_ClientAbortedNo500(t *testing.T) {
	reader := &stubIssuersReader{
		listErr: errors.New("pq: canceling statement due to user request"),
	}
	srv := v1.New(v1.Options{Issuers: reader})

	req := httptest.NewRequest(http.MethodGet, "/v1/issuers", nil)
	ctx, cancel := context.WithCancel(req.Context())
	cancel() // client went away before/while the handler ran
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code == http.StatusInternalServerError {
		t.Fatalf("client-aborted request must NOT return 500; got %d body=%s",
			rec.Code, rec.Body.String())
	}
}

// ─── /v1/issuers/{g_strkey} (detail) ──────────────────────────────

// TestHandleIssuer_503WhenReaderNil — same gate as the listing.
func TestHandleIssuer_503WhenReaderNil(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// TestHandleIssuer_NotFound404 — sql.ErrNoRows surfaces as 404
// with `issuer-not-found` problem type; the handler must distinguish
// ErrNoRows from generic
// storage failures (which 500).
func TestHandleIssuer_NotFound404(t *testing.T) {
	reader := &stubIssuersReader{rowErr: sql.ErrNoRows}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/GFAKEUNKNOWNUNKNOWNUNKNOWNUNKNOWNUNKNOWNUNKNOWNUNKNOWNUNKNOWN")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, "issuer-not-found") {
		t.Errorf("expected `issuer-not-found` problem type, got: %s", body)
	}
}

// TestHandleIssuer_LowercaseInputUppercased — the handler upper-
// cases the path segment before hitting storage so URL clients
// that auto-lowercase don't dead-end: `/v1/issuers/ga5zsejyb...` must not
// 404 while the uppercase form returns the row. Verified by checking what
// the storage stub received.
func TestHandleIssuer_LowercaseInputUppercased(t *testing.T) {
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{
			GStrkey: "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
		},
	}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	_ = mustGet(t, ts.URL+"/v1/issuers/ga5zsejyb37jrc5avcia5mop4rhtm335x2kgx3ihojapp5re34k4kzvn")
	if reader.lastGStrkey != "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN" {
		t.Errorf("storage saw %q, want uppercased form", reader.lastGStrkey)
	}
}

// TestHandleIssuer_HappyPath_WithAssets — full row decode, plus
// the per-issuer assets list flowing through.
func TestHandleIssuer_HappyPath_WithAssets(t *testing.T) {
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{
			GStrkey:    "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			HomeDomain: "centre.io",
			OrgName:    "Circle",
		},
		assets: []timescale.IssuerAsset{
			{
				AssetID:          "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
				Code:             "USDC",
				Slug:             "USDC",
				FirstSeenLedger:  10000000,
				LastSeenLedger:   62500000,
				ObservationCount: 41610623,
			},
		},
	}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.OrgName != "Circle" {
		t.Errorf("OrgName = %q", env.Data.OrgName)
	}
	if len(env.Data.Assets) != 1 {
		t.Fatalf("len(assets) = %d, want 1", len(env.Data.Assets))
	}
	a := env.Data.Assets[0]
	if a.Code != "USDC" || a.ObservationCount != 41610623 {
		t.Errorf("asset = %+v", a)
	}
}

// TestHandleIssuer_ScamSuppressesSEP1Payload — scam-identity suppression must
// clear SEP1Payload alongside HomeDomain/OrgName. The raw
// stellar.toml JSONB carries the same impersonated org_name/
// home_domain the two string fields are cleared of; served verbatim,
// a client decoding sep1_payload would recover the
// exact identity the suppression exists to hide.
func TestHandleIssuer_ScamSuppressesSEP1Payload(t *testing.T) {
	const counterfeiter = "GBYBVWOOVC4EJVRIF4HMWG5B7POLCS7JRPY5KYR3BCLEK24IJQOGUARD"
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{
			GStrkey:     counterfeiter,
			HomeDomain:  "lobstr.co",
			OrgName:     "LOBSTR",
			OrgVerified: false,
			SEP1Payload: []byte(`{"DOCUMENTATION":{"ORG_NAME":"LOBSTR"}}`),
		},
	}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/"+counterfeiter)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	body, _ := readAll(resp)
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.ScamReason == "" {
		t.Fatal("fixture g_strkey didn't trip the scam gate — test no longer exercises S-010")
	}
	if env.Data.HomeDomain != "" || env.Data.OrgName != "" {
		t.Errorf("suppression didn't clear HomeDomain/OrgName: %+v", env.Data)
	}
	if len(env.Data.SEP1Payload) != 0 {
		t.Errorf("SEP1Payload = %s, want cleared — the raw toml still embeds the impersonated identity", env.Data.SEP1Payload)
	}
}

// TestHandleIssuer_AssetsSoftFail — when ListIssuerAssets fails,
// the issuer card still renders without it. The handler logs at
// WARN and proceeds with assets = nil. Critical for explorer UX:
// a failure to load the per-asset list shouldn't 500 the whole
// issuer detail page. It must also carry a coverage_note so a
// failed read is distinguishable on the wire from a genuine
// zero-asset issuer (CA2-A04-harden-9).
func TestHandleIssuer_AssetsSoftFail(t *testing.T) {
	reader := &stubIssuersReader{
		row: timescale.IssuerRow{
			GStrkey:    "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			HomeDomain: "centre.io",
		},
		assetsE: errors.New("assets fetch broke"),
	}
	srv := v1.New(v1.Options{Issuers: reader})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/issuers/GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 (asset-list failure should soft-fail)", resp.StatusCode)
	}
	body, _ := readAll(resp)
	if !strings.Contains(body, `"home_domain":"centre.io"`) {
		t.Errorf("issuer body missing despite soft-fail path: %s", body)
	}
	var env struct {
		Data v1.Issuer `json:"data"`
	}
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&env); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, body)
	}
	if env.Data.CoverageNote == "" {
		t.Error("coverage_note should be set when ListIssuerAssets fails, " +
			"so a failed read isn't byte-identical to a zero-asset issuer")
	}
	if len(env.Data.Assets) != 0 {
		t.Errorf("Assets = %+v, want empty on soft-fail", env.Data.Assets)
	}
}

// TestHandleIssuer_ServesThePersistedProvenance — a recovered reading is only
// safe to serve because it arrives LABELLED. If the handler drops the label,
// a client sees four auth flags with no way to tell that they describe an
// account which has been removed, which is a quieter defect than the
// unresolved row it replaced.
func TestHandleIssuer_ServesThePersistedProvenance(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		AuthRequired:        boolPtr(false),
		AuthRevocable:       boolPtr(true),
		AuthImmutable:       boolPtr(false),
		AuthClawback:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLastKnownBeforeRemoval),
		AuthFlagsAsOfLedger: u32(54564588),
	}}
	// The account really is gone, so the live reader resolves nothing and
	// the persisted reading must stand — labelled.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: false}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+mergedIssuerG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "last_known_before_removal" {
		t.Errorf("auth_flags_source = %q, want %q — the persisted label must reach the wire",
			env.Data.AuthFlagsSource, "last_known_before_removal")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 54564588 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 54564588 (its removal ledger)", env.Data.AuthFlagsAsOfLedger)
	}
	// The recovered VALUES still have to be right — mask 0xA.
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("auth_revocable = %v, want true", env.Data.AuthRevocable)
	}
	if env.Data.AuthClawback == nil || !*env.Data.AuthClawback {
		t.Errorf("auth_clawback = %v, want true", env.Data.AuthClawback)
	}
}

// TestHandleIssuer_PersistedLastKnownIsReofferedToTheLiveReader — a
// `last_known_before_removal` row stops being true the moment the account is
// re-created at the same address, and the drain's primary queue
// (`auth_required IS NULL`) never revisits a filled row, so the read path
// must re-offer it to the live reader.
func TestHandleIssuer_PersistedLastKnownIsReofferedToTheLiveReader(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "sep1-sourced.example",
		AuthRequired:        boolPtr(true),
		AuthRevocable:       boolPtr(true),
		AuthImmutable:       boolPtr(true),
		AuthClawback:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLastKnownBeforeRemoval),
		AuthFlagsAsOfLedger: u32(54564588),
	}}
	// Re-created at the same address, every flag cleared.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0,
		LastModifiedLedger: 64228661,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+mergedIssuerG)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q — a last-known row must be re-offered to the live reader, not frozen",
			env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64228661 (the re-created entry's ledger)", env.Data.AuthFlagsAsOfLedger)
	}
	for _, f := range []struct {
		name string
		got  *bool
	}{
		{"auth_required", env.Data.AuthRequired},
		{"auth_revocable", env.Data.AuthRevocable},
		{"auth_immutable", env.Data.AuthImmutable},
		{"auth_clawback", env.Data.AuthClawback},
	} {
		if f.got == nil || *f.got {
			t.Errorf("%s = %v, want false — the re-created account's own entry outranks the pre-removal reading", f.name, f.got)
		}
	}
}

// TestHandleIssuer_PersistedLiveReadingIsReReadThroughThePointLookup — a
// filled `live` row is re-read too (it can hold a domain the account has since
// moved away from), and it is re-read through the narrow key_xdr point lookup
// rather than AccountStateCached, whose fan-out to trustlines and offers would
// put a cold lake read on every issuer page.
func TestHandleIssuer_PersistedLiveReadingIsReReadThroughThePointLookup(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:             mergedIssuerG,
		HomeDomain:          "centre.io",
		AuthRequired:        boolPtr(true),
		AuthFlagsSource:     string(clickhouse.AuthFlagsSourceLive),
		AuthFlagsAsOfLedger: u32(64100000),
	}}
	flags := &stubIssuerAuthFlags{live: map[string]clickhouse.AccountAuthFlags{
		mergedIssuerG: {Required: false, HomeDomain: "centre.io", Source: clickhouse.AuthFlagsSourceLive, AsOfLedger: 64228661},
	}}
	// A different answer than the point lookup's, so the response shows which
	// reader it came from.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0x1,
		LastModifiedLedger: 64999999,
	}}
	got := getIssuer(t, v1.Options{Issuers: reader, Explorer: explorer, IssuerAuthFlags: flags}, mergedIssuerG)

	if len(flags.calls) != 1 || flags.calls[0] != mergedIssuerG {
		t.Fatalf("point lookup calls = %v, want exactly [%s]", flags.calls, mergedIssuerG)
	}
	if got.AuthFlagsAsOfLedger == nil || *got.AuthFlagsAsOfLedger != 64228661 {
		t.Errorf("auth_flags_as_of_ledger = %v, want the point lookup's 64228661", got.AuthFlagsAsOfLedger)
	}
	if got.AuthRequired == nil || *got.AuthRequired {
		t.Errorf("auth_required = %v, want the point lookup's false", got.AuthRequired)
	}
}

// TestHandleIssuer_LiveAccountStateStampsLiveProvenance — when the enrichment
// resolves a LIVE AccountEntry, the response must say so and pin the ledger
// the reading is true as of. Without that a consumer cannot tell a current
// policy from a last-known one, which is the whole point of being able to
// resolve the merged issuers at all.
func TestHandleIssuer_LiveAccountStateStampsLiveProvenance(t *testing.T) {
	reader := &stubIssuersReader{row: timescale.IssuerRow{GStrkey: provenanceIssuer}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0xA, // AUTH_REVOCABLE | AUTH_CLAWBACK
		HomeDomain:         "live-onchain.example",
		LastModifiedLedger: 64100000,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q", env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64100000 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64100000 (the entry's last-modified ledger)",
			env.Data.AuthFlagsAsOfLedger)
	}
	if env.Data.AuthRequired == nil || *env.Data.AuthRequired {
		t.Errorf("auth_required = %v, want false (mask 0xA)", env.Data.AuthRequired)
	}
	if env.Data.AuthRevocable == nil || !*env.Data.AuthRevocable {
		t.Errorf("auth_revocable = %v, want true (mask 0xA)", env.Data.AuthRevocable)
	}
	if env.Data.AuthClawback == nil || !*env.Data.AuthClawback {
		t.Errorf("auth_clawback = %v, want true (mask 0xA)", env.Data.AuthClawback)
	}
}

// TestHandleIssuer_LiveAccountEntryOutranksPersistedFlags — the re-creation
// case. Once the drain can persist a merged issuer's last-known flags, a row
// whose account has been RE-CREATED on-chain must resolve back to its live
// values and be labelled `live`. The account's own current AccountEntry is
// the authority on its own policy; the persisted column is a cache of it, and
// the drain's queue (`auth_required IS NULL`) will never revisit the row to
// correct it.
func TestHandleIssuer_LiveAccountEntryOutranksPersistedFlags(t *testing.T) {
	stale := true
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:       provenanceIssuer,
		AuthRequired:  &stale,
		AuthRevocable: &stale,
		AuthImmutable: &stale,
		AuthClawback:  &stale,
	}}
	// Re-created account: every flag cleared.
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{
		Exists:             true,
		Flags:              0,
		LastModifiedLedger: 64212818,
	}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	for _, f := range []struct {
		name string
		got  *bool
	}{
		{"auth_required", env.Data.AuthRequired},
		{"auth_revocable", env.Data.AuthRevocable},
		{"auth_immutable", env.Data.AuthImmutable},
		{"auth_clawback", env.Data.AuthClawback},
	} {
		if f.got == nil || *f.got {
			t.Errorf("%s = %v, want false — the live AccountEntry outranks the persisted value", f.name, f.got)
		}
	}
	if env.Data.AuthFlagsSource != "live" {
		t.Errorf("auth_flags_source = %q, want %q", env.Data.AuthFlagsSource, "live")
	}
	if env.Data.AuthFlagsAsOfLedger == nil || *env.Data.AuthFlagsAsOfLedger != 64212818 {
		t.Errorf("auth_flags_as_of_ledger = %v, want 64212818", env.Data.AuthFlagsAsOfLedger)
	}
}

// TestHandleIssuer_NoLiveEntryNeverClaimsProvenance — absence from the
// current-state projection is what a MERGED account and a lake-coverage gap
// BOTH look like (r1's projection holds no `removed` row below ledger
// 38,000,000 at all, so an account merged before that is simply missing). The
// read path may therefore neither upgrade a persisted reading to `live` nor
// conclude `last_known_before_removal` on its own: it leaves the flags alone
// and says nothing about them. Only the drain, which reads an actual
// `removed` row and its removal ledger, may write the historical label.
func TestHandleIssuer_NoLiveEntryNeverClaimsProvenance(t *testing.T) {
	persisted := true
	reader := &stubIssuersReader{row: timescale.IssuerRow{
		GStrkey:      provenanceIssuer,
		AuthRequired: &persisted,
	}}
	explorer := &stubExplorerReader{accountState: clickhouse.AccountState{Exists: false}}
	srv := v1.New(v1.Options{Issuers: reader, Explorer: explorer})
	ts := startHTTPTest(t, srv.Handler())

	var env struct {
		Data v1.Issuer `json:"data"`
	}
	resp := mustGet(t, ts.URL+"/v1/issuers/"+provenanceIssuer)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	mustDecode(t, resp, &env)

	if env.Data.AuthFlagsSource != "" {
		t.Errorf("auth_flags_source = %q, want empty — no live entry resolved, so nothing is known about provenance",
			env.Data.AuthFlagsSource)
	}
	if env.Data.AuthFlagsAsOfLedger != nil {
		t.Errorf("auth_flags_as_of_ledger = %v, want absent", env.Data.AuthFlagsAsOfLedger)
	}
	if env.Data.AuthRequired == nil || !*env.Data.AuthRequired {
		t.Errorf("auth_required = %v, want the persisted true left untouched", env.Data.AuthRequired)
	}
}
