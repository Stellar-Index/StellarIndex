package blnt

import (
	"encoding/base64"
	"errors"
	"reflect"
	"testing"

	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

func testEvent(contractID, kind string) events.Event {
	return events.Event{
		Ledger:         GenesisLedger,
		LedgerClosedAt: "2026-10-10T00:00:00Z",
		ContractID:     contractID,
		TxHash:         "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Topic:          []string{scval.MustEncodeSymbol(kind)},
		Value:          scval.MustEncodeSymbol("body"),
	}
}

func TestDecoderMatchesOnlyRoleSpecificCanonicalEvents(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	tests := []struct {
		name     string
		contract string
		kind     string
		want     bool
	}{
		{"pool supply", MainnetFixedPool, "supply", true},
		{"second pool clawback", MainnetYieldBlentPool, "clawback", true},
		{"factory deploy", MainnetPoolFactory, "deploy", true},
		{"backstop tier deposit", MainnetBackstop, "deposit", true},
		{"emitter distribute", MainnetEmitter, "distribute", true},
		{"backfill conversion", MainnetBackfill, "swap_blnd", true},
		{"lockup funding", MainnetLockupBackfill, "fund", true},
		{"pool symbol on backfill", MainnetBackfill, "supply", false},
		{"backfill symbol on pool", MainnetFixedPool, "swap_blnd", false},
		{"foreign lookalike", "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABSC4", "supply", false},
		{"unknown symbol", MainnetFixedPool, "not_blnt", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := d.Matches(testEvent(tc.contract, tc.kind)); got != tc.want {
				t.Errorf("Matches(%s, %s) = %v, want %v", tc.contract, tc.kind, got, tc.want)
			}
		})
	}
}

func TestABIClassifiesEveryDeclaredKind(t *testing.T) {
	t.Parallel()
	for role, kinds := range abi {
		for kind := range kinds {
			ev := testEvent(MainnetFixedPool, kind)
			if got := classify(role, &ev); got != kind {
				t.Errorf("classify(%s, %s) = %q", role, kind, got)
			}
		}
	}
}

// The kinds below have no chain event yet, so their specs rest on the contract source alone; these
// pin the three body shapes (vec with attribute amounts, unit, optional trailing topic) by hand.
func TestDecodeSourceOnlyShapes(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	asset, assetKey := encodedContractAddress(t, 0x11)
	from, fromKey := encodedContractAddress(t, 0x22)
	// 2^100: an amount an int64 cannot hold (invariant 1).
	big := sdkxdr.Int128Parts{Hi: 1 << 36, Lo: 0}

	claw := testEvent(MainnetFixedPool, "clawback")
	claw.Topic = append(claw.Topic, asset, from)
	claw.Value = encodeVec(t, i128(big), i128(sdkxdr.Int128Parts{Lo: 7}), i128(sdkxdr.Int128Parts{Lo: 9}))
	got := mustDecode(t, d, claw)
	if got.Pool != MainnetFixedPool || got.Asset != assetKey || got.UserAddress != fromKey ||
		got.Amount != "1267650600228229401496703205376" ||
		got.Attributes["supply_burned"] != "7" || got.Attributes["collateral_burned"] != "9" {
		t.Errorf("clawback decoded to %+v", got)
	}

	del := testEvent(MainnetFixedPool, "delete_auction")
	del.Topic = append(del.Topic, encode(t, u32(3)), from)
	del.Value = encode(t, sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvVoid})
	got = mustDecode(t, d, del)
	if got.UserAddress != fromKey || got.Attributes["auction_type"] != uint32(3) {
		t.Errorf("delete_auction decoded to %+v", got)
	}

	status := testEvent(MainnetFixedPool, "set_status")
	status.Value = encode(t, u32(1))
	got = mustDecode(t, d, status)
	if _, hasAdmin := got.Attributes["admin"]; hasAdmin || got.Attributes["status"] != uint32(1) {
		t.Errorf("admin-less set_status decoded to %+v", got)
	}
}

// One hand-encoded event for each remaining source-only shape, with a distinct value per field so a
// swapped or renamed field fails. Kinds that only repeat the position shape are left to the real fixtures.
func TestDecodeSourceOnlyFieldOrder(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	asset, assetKey := encodedContractAddress(t, 0x11)
	who, whoKey := encodedContractAddress(t, 0x22)
	pool, poolKey := encodedContractAddress(t, 0x33)
	n := func(v uint64) sdkxdr.ScVal { return i128(sdkxdr.Int128Parts{Lo: sdkxdr.Uint64(v)}) }
	tier := encodeVec(t, sym("SecondLoss"))

	cases := []struct {
		contract, kind string
		topics         []string
		body           string
		want           Event
	}{
		{
			MainnetFixedPool, "update_pool",
			[]string{who},
			encodeVec(t, u32(1), u32(2), n(3)),
			Event{Pool: MainnetFixedPool, Attributes: map[string]any{"admin": whoKey, "backstop_take_rate": uint32(1), "max_positions": uint32(2), "min_collateral": "3"}},
		},
		{
			MainnetFixedPool, "reconcile_loss",
			[]string{asset},
			encodeVec(t, n(1), n(2), n(3), n(4)),
			Event{Pool: MainnetFixedPool, Asset: assetKey, Amount: "1", Attributes: map[string]any{"supplier_loss": "2", "backstop_credit_loss": "3", "b_rate_loss": "4"}},
		},
		{
			MainnetFixedPool, "bad_debt",
			[]string{who, asset},
			encode(t, n(5)),
			Event{Pool: MainnetFixedPool, UserAddress: whoKey, Asset: assetKey, Amount: "5", Attributes: map[string]any{}},
		},
		{
			MainnetBackstop, "draw",
			[]string{tier, pool},
			encodeVec(t, mustAddr(t, 0x22), n(6)),
			Event{Pool: poolKey, Tier: "SecondLoss", Amount: "6", Attributes: map[string]any{"to": whoKey}},
		},
		{
			MainnetBackstop, "withdraw",
			[]string{tier, pool, who},
			encodeVec(t, n(7), n(8)),
			Event{Pool: poolKey, UserAddress: whoKey, Tier: "SecondLoss", Amount: "8", Amount2: "7", Attributes: map[string]any{}},
		},
		{
			MainnetBackfill, "burn_expired", nil, encodeVec(t, n(9), n(10)),
			Event{Amount: "9", Attributes: map[string]any{"total_blnt_burned": "10"}},
		},
		{
			MainnetLockupBackfill, "claim",
			[]string{who},
			encodeVec(t, n(11), n(12)),
			Event{UserAddress: whoKey, Amount: "11", Attributes: map[string]any{"total_claimed": "12"}},
		},
		{
			MainnetLockupBackfill, "expire",
			[]string{who},
			encodeVec(t, n(13), n(14)),
			Event{UserAddress: whoKey, Amount: "13", Attributes: map[string]any{"total_claimed": "14"}},
		},
	}
	for _, tc := range cases {
		ev := testEvent(tc.contract, tc.kind)
		ev.Topic = append(ev.Topic, tc.topics...)
		ev.Value = tc.body
		got := mustDecode(t, d, ev)
		got = Event{
			Pool: got.Pool, UserAddress: got.UserAddress, Asset: got.Asset, Tier: got.Tier,
			Amount: got.Amount, Amount2: got.Amount2, Attributes: got.Attributes,
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", tc.kind, got, tc.want)
		}
	}
}

func mustAddr(t *testing.T, fill byte) sdkxdr.ScVal {
	t.Helper()
	b64, _ := encodedContractAddress(t, fill)
	sv, err := scval.Parse(b64)
	if err != nil {
		t.Fatal(err)
	}
	return sv
}

// A shape the ABI does not declare must fail, never land a partial row.
func TestDecodeRejectsUndeclaredShapes(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	asset, _ := encodedContractAddress(t, 0x11)
	from, _ := encodedContractAddress(t, 0x22)
	one := i128(sdkxdr.Int128Parts{Lo: 1})

	supply := func() events.Event {
		ev := testEvent(MainnetFixedPool, "supply")
		ev.Topic = append(ev.Topic, asset, from)
		ev.Value = encodeVec(t, one, one)
		return ev
	}
	mustDecode(t, d, supply())

	cases := map[string]func(*events.Event){
		"missing topic":   func(ev *events.Event) { ev.Topic = ev.Topic[:2] },
		"extra topic":     func(ev *events.Event) { ev.Topic = append(ev.Topic, from) },
		"short body":      func(ev *events.Event) { ev.Value = encodeVec(t, one) },
		"long body":       func(ev *events.Event) { ev.Value = encodeVec(t, one, one, one) },
		"wrong type":      func(ev *events.Event) { ev.Value = encodeVec(t, one, u32(1)) },
		"topic not addr":  func(ev *events.Event) { ev.Topic[1] = encode(t, u32(1)) },
		"body not base64": func(ev *events.Event) { ev.Value = "not-base64" },
	}
	for name, mutate := range cases {
		ev := supply()
		mutate(&ev)
		if _, err := d.Decode(ev); !errors.Is(err, ErrMalformedEvent) {
			t.Errorf("%s: Decode error = %v, want ErrMalformedEvent", name, err)
		}
	}

	del := testEvent(MainnetFixedPool, "delete_auction")
	del.Topic = append(del.Topic, encode(t, u32(0)), from)
	del.Value = encodeVec(t, one)
	if _, err := d.Decode(del); !errors.Is(err, ErrMalformedEvent) {
		t.Errorf("unit event with a payload: Decode error = %v, want ErrMalformedEvent", err)
	}

	tier := testEvent(MainnetBackstop, "donate")
	pool, _ := encodedContractAddress(t, 0x33)
	tier.Topic = append(tier.Topic, encodeVec(t, sym("FourthLoss")), pool, from)
	tier.Value = encode(t, one)
	if _, err := d.Decode(tier); !errors.Is(err, ErrMalformedEvent) {
		t.Errorf("unknown tier: Decode error = %v, want ErrMalformedEvent", err)
	}
}

func mustDecode(t *testing.T, d *Decoder, ev events.Event) Event {
	t.Helper()
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode(%s): %v", ev.Topic[0], err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	return out[0].(Event)
}

func i128(p sdkxdr.Int128Parts) sdkxdr.ScVal {
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvI128, I128: &p}
}

func u32(n uint32) sdkxdr.ScVal {
	v := sdkxdr.Uint32(n)
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvU32, U32: &v}
}

func sym(s string) sdkxdr.ScVal {
	v := sdkxdr.ScSymbol(s)
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &v}
}

func encodeVec(t *testing.T, vals ...sdkxdr.ScVal) string {
	t.Helper()
	vec := sdkxdr.ScVec(vals)
	pv := &vec
	return encode(t, sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvVec, Vec: &pv})
}

func encode(t *testing.T, sv sdkxdr.ScVal) string {
	t.Helper()
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestFactoryDeployAdmitsChildPool(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	body, child := encodedContractAddress(t, 0x42)
	deploy := testEvent(MainnetPoolFactory, "deploy")
	deploy.Value = body
	if _, err := d.Decode(deploy); err != nil {
		t.Fatalf("Decode(deploy): %v", err)
	}
	if !d.Matches(testEvent(child, "supply")) {
		t.Fatalf("factory child %s was not admitted as a pool", child)
	}
}

func encodedContractAddress(t *testing.T, fill byte) (string, string) {
	t.Helper()
	var cid sdkxdr.ContractId
	for i := range cid {
		cid[i] = fill
	}
	addr := sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal contract address: %v", err)
	}
	b64 := base64.StdEncoding.EncodeToString(raw)
	parsed, err := scval.Parse(b64)
	if err != nil {
		t.Fatalf("parse encoded address: %v", err)
	}
	strkey, err := scval.AsAddressStrkey(parsed)
	if err != nil {
		t.Fatalf("strkey encoded address: %v", err)
	}
	return b64, strkey
}

// TxIndex is the event's 0-based apply position, and an event without one (0 = unknown) stays nil, never position 0.
func TestDecodeCarriesTxIndex(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	ev := testEvent(MainnetFixedPool, "set_status")
	ev.Value = encode(t, u32(1))
	if got := mustDecode(t, d, ev); got.TxIndex != nil {
		t.Errorf("TxIndex without an apply order = %d, want nil", *got.TxIndex)
	}
	for _, tc := range []struct {
		in   int
		want uint32
	}{{1, 0}, {7, 6}} {
		ev.TransactionIndex = tc.in
		if got := mustDecode(t, d, ev); got.TxIndex == nil || *got.TxIndex != tc.want {
			t.Errorf("TransactionIndex %d decoded to TxIndex %v, want %d", tc.in, got.TxIndex, tc.want)
		}
	}
}
