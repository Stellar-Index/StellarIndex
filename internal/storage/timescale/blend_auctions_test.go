package timescale

import (
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/domain"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

// canonicalSorobanAsset is a small helper because the test
// fixtures are built with a known-good Soroban contract id (32-byte
// hex prefix → C-strkey).
func canonicalSorobanAsset(t *testing.T, strkeyAddr string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewSorobanAsset(strkeyAddr)
	if err != nil {
		t.Fatalf("NewSorobanAsset(%q): %v", strkeyAddr, err)
	}
	return a
}

func TestEncodeBlendAssetAmounts_RoundTrip(t *testing.T) {
	// Two-token bid map.
	asset1 := canonicalSorobanAsset(t, "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA") // XLM SAC
	asset2 := canonicalSorobanAsset(t, "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75") // some other Soroban contract
	in := []blend.AssetAmount{
		{Asset: asset1, Amount: big.NewInt(1_000_000_000_000)}, // 12-digit i128 — well over int64 ceiling for a stress test
		{Asset: asset2, Amount: big.NewInt(7)},
	}
	encoded, err := encodeBlendAssetAmounts(in)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(encoded) == 0 {
		t.Fatal("encoded is empty")
	}
	if !strings.Contains(string(encoded), "1000000000000") {
		t.Errorf("encoded JSON missing first amount: %s", encoded)
	}

	out, err := decodeBlendAssetAmounts(string(encoded))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("decoded len=%d want 2: %+v", len(out), out)
	}
	if out[0].Amount != "1000000000000" {
		t.Errorf("out[0].Amount=%q want \"1000000000000\"", out[0].Amount)
	}
	if out[1].Asset != asset2.String() {
		t.Errorf("out[1].Asset=%q want %q", out[1].Asset, asset2.String())
	}
}

func TestEncodeBlendAssetAmounts_EmptyReturnsNil(t *testing.T) {
	// An empty slice encodes to SQL NULL: encodeBlendAssetAmounts returns (nil, nil) for empty input rather than
	// a '[]' JSON array, so delete-event rows have a genuinely absent column.
	encoded, err := encodeBlendAssetAmounts(nil)
	if err != nil {
		t.Fatalf("encode nil: %v", err)
	}
	if encoded != nil {
		t.Errorf("encode nil → %q, want nil", encoded)
	}
	encoded, err = encodeBlendAssetAmounts([]blend.AssetAmount{})
	if err != nil {
		t.Fatalf("encode empty: %v", err)
	}
	if encoded != nil {
		t.Errorf("encode empty → %q, want nil", encoded)
	}
}

func TestDecodeBlendAssetAmounts_EmptyString(t *testing.T) {
	out, err := decodeBlendAssetAmounts("")
	if err != nil {
		t.Fatalf("decode empty: %v", err)
	}
	if out != nil {
		t.Errorf("decode \"\" → %+v, want nil", out)
	}
}

func TestDecodeBlendAssetAmounts_HugeI128(t *testing.T) {
	// Validate full i128 precision through the JSON boundary —
	// 39 digit number is the max-i128 ceiling order (170141183…).
	huge := "170141183460469231731687303715884105727" // 2^127 - 1
	js := `[{"asset":"CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA","amount":"` + huge + `"}]`
	out, err := decodeBlendAssetAmounts(js)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out) != 1 || out[0].Amount != huge {
		t.Errorf("out=%+v, want one row with amount=%q", out, huge)
	}
}

func TestDecodeBlendAssetAmounts_BadJSON(t *testing.T) {
	_, err := decodeBlendAssetAmounts("{not valid json")
	if err == nil {
		t.Fatal("expected error on invalid JSON")
	}
	if !strings.Contains(err.Error(), "unmarshal") {
		t.Errorf("err=%v missing 'unmarshal'", err)
	}
}

// A V1 pool's queue_set_reserve persists its partial config plus the
// names of the V2-only fields it lacked, and the persisted metadata
// reads back through the APY config reader.
func TestBuildAdminAttributes_V1QueueSetReserve(t *testing.T) {
	e := domain.BlendAdminEvent{
		Kind: domain.BlendEventQueueSetReserve,
		ReserveConfig: map[string]any{
			"index": uint64(1), "decimals": uint64(7), "util": uint64(8_000_000),
			"r_three": uint64(15_000_000), "reactivity": uint64(200),
		},
		ReserveConfigMissing: []string{"supply_cap", "enabled"},
	}
	b, err := json.Marshal(buildAdminAttributes(e))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got struct {
		Metadata json.RawMessage `json:"metadata"`
		Missing  []string        `json:"reserve_config_missing"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal %s: %v", b, err)
	}
	if want := []string{"supply_cap", "enabled"}; !reflect.DeepEqual(got.Missing, want) {
		t.Errorf("reserve_config_missing = %v, want %v (attrs %s)", got.Missing, want, b)
	}
	cfg, err := blend.ParseReserveConfigMetadata(got.Metadata)
	if err != nil {
		t.Fatalf("ParseReserveConfigMetadata: %v", err)
	}
	if cfg.Util != 8_000_000 || cfg.RThree != 15_000_000 || !cfg.Enabled || cfg.SupplyCap != nil {
		t.Errorf("read-back config = %+v, want V1 params, enabled, nil cap", cfg)
	}

	e.ReserveConfigMissing = nil
	if _, ok := buildAdminAttributes(e)["reserve_config_missing"]; ok {
		t.Error("complete config must not carry reserve_config_missing")
	}
}

// validBlendPositionEvent returns an otherwise-well-formed event so a
// test can null/negate a single amount field in isolation.
func validBlendPositionEvent() domain.BlendPositionEvent {
	return domain.BlendPositionEvent{
		Pool:        "CPOOL",
		Kind:        domain.BlendEventSupply,
		Asset:       "CASSET",
		User:        "GUSER",
		TokenAmount: big.NewInt(1),
		BOrDAmount:  big.NewInt(1),
		Ledger:      100,
		TxHash:      "abc",
		OpIndex:     0,
		EventIndex:  0,
		Timestamp:   time.Unix(1, 0).UTC(),
	}
}

// TestInsertBlendPositionEvent_RejectsBadAmount: token_amount and b_or_d_amount need a SQL-boundary
// validator, else a nil amount is coerced to "0" and a negative one written verbatim. The guard must reject
// both before touching the DB; s.db is nil, so reaching ExecContext would panic.
func TestInsertBlendPositionEvent_RejectsBadAmount(t *testing.T) {
	s := &Store{}
	ctx := context.Background()

	cases := []struct {
		name    string
		mutate  func(*domain.BlendPositionEvent)
		wantSub string
	}{
		{"nil TokenAmount", func(e *domain.BlendPositionEvent) { e.TokenAmount = nil }, "TokenAmount is nil"},
		{"negative TokenAmount", func(e *domain.BlendPositionEvent) { e.TokenAmount = big.NewInt(-1) }, "TokenAmount must be >= 0"},
		{"nil BOrDAmount", func(e *domain.BlendPositionEvent) { e.BOrDAmount = nil }, "BOrDAmount is nil"},
		{"negative BOrDAmount", func(e *domain.BlendPositionEvent) { e.BOrDAmount = big.NewInt(-1) }, "BOrDAmount must be >= 0"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := validBlendPositionEvent()
			tc.mutate(&e)
			err := s.InsertBlendPositionEvent(ctx, e)
			if err == nil {
				t.Fatalf("InsertBlendPositionEvent(%s) = nil error, want rejection", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantSub)
			}
		})
	}
}
