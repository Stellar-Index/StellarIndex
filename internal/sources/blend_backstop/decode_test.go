package blend_backstop

import (
	"encoding/base64"
	"errors"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── real-lake golden frames (base64 SCVal) ──────────────────────
//
// Captured from real mainnet ledgers; the contract id is the V2
// backstop. These PIN the reverse-engineered schemas: if a decode
// helper drifts, the asserted promoted fields change.

const goldenContractV2 = MainnetBackstopV2

var goldenFrames = map[string]struct {
	topics []string
	data   string
}{
	"deposit": {
		topics: []string{
			"AAAADwAAAAdkZXBvc2l0AA==",
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
			"AAAAEgAAAAAAAAAAWYH8JZJI+MYX5VM6OjGDm14Ek5xVtC/UzF0n7glLQkw=",
		},
		data: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAAAqxpkoAAAAKAAAAAAAAAAAAAAAAB88kBQ==",
	},
	"claim": {
		topics: []string{
			"AAAADwAAAAVjbGFpbQAAAA==",
			"AAAAEgAAAAAAAAAAWYH8JZJI+MYX5VM6OjGDm14Ek5xVtC/UzF0n7glLQkw=",
		},
		data: "AAAACgAAAAAAAAAAAAAAAAqxpko=",
	},
	"distribute": {
		topics: []string{"AAAADwAAAApkaXN0cmlidXRlAAA="},
		data:   "AAAACgAAAAAAAAAAAAABjj4rKgA=",
	},
	"queue_withdrawal": {
		topics: []string{
			"AAAADwAAABBxdWV1ZV93aXRoZHJhd2Fs",
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
			"AAAAEgAAAAAAAAAAWYH8JZJI+MYX5VM6OjGDm14Ek5xVtC/UzF0n7glLQkw=",
		},
		data: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAACuGk0QAAAAFAAAAAGpGyk0=",
	},
	// withdraw — real V2 lake sample, ledger 57072018 (2025-05-14).
	// Pins bug #6: body element order is (shares_burned, tokens_out),
	// the OPPOSITE of deposit's (tokens_in, shares_minted).
	"withdraw": {
		topics: []string{
			"AAAADwAAAAh3aXRoZHJhdw==",
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
			"AAAAEgAAAAAAAAAAHtnvX/8wUxWSVA274d00nkNvc5RhRseahDuo6YS0g5Q=",
		},
		data: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAAAAADGXUAAAAAKAAAAAAAAAAAAAAAAAMbVEA==",
	},
}

func goldenEvent(t *testing.T, name string) *events.Event {
	t.Helper()
	f, ok := goldenFrames[name]
	if !ok {
		t.Fatalf("no golden frame %q", name)
	}
	return &events.Event{
		Type:           "contract",
		ContractID:     goldenContractV2,
		Ledger:         56_700_000,
		LedgerClosedAt: "2026-06-15T00:00:00Z",
		TxHash:         "deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef0",
		Topic:          f.topics,
		Value:          f.data,
	}
}

// TestGolden_Deposit pins the deposit decode against the real lake
// sample — the canonical sanity check for the two-i128 body shape.
func TestGolden_Deposit(t *testing.T) {
	t.Parallel()
	d, err := decodeDeposit(goldenEvent(t, "deposit"))
	if err != nil {
		t.Fatalf("decodeDeposit: %v", err)
	}
	if d.Amount != "179414602" {
		t.Errorf("Amount = %q, want 179414602", d.Amount)
	}
	if d.Amount2 != "131015685" {
		t.Errorf("Amount2 (shares) = %q, want 131015685", d.Amount2)
	}
	if d.Pool == "" || d.Pool[0] != 'C' {
		t.Errorf("Pool should be a contract strkey, got %q", d.Pool)
	}
	if d.UserAddress == "" || d.UserAddress[0] != 'G' {
		t.Errorf("UserAddress should be an account strkey, got %q", d.UserAddress)
	}
}

// TestGolden_Claim — claim carries a user + amount and NO pool.
func TestGolden_Claim(t *testing.T) {
	t.Parallel()
	d, err := decodeClaim(goldenEvent(t, "claim"))
	if err != nil {
		t.Fatalf("decodeClaim: %v", err)
	}
	if d.Amount != "179414602" {
		t.Errorf("Amount = %q, want 179414602", d.Amount)
	}
	if d.Pool != "" {
		t.Errorf("claim should carry no pool, got %q", d.Pool)
	}
	if d.UserAddress == "" {
		t.Error("claim should carry a user")
	}
}

// TestGolden_Distribute — single i128 amount, no pool/user.
func TestGolden_Distribute(t *testing.T) {
	t.Parallel()
	d, err := decodeDistribute(goldenEvent(t, "distribute"))
	if err != nil {
		t.Fatalf("decodeDistribute: %v", err)
	}
	if d.Amount != "1710440000000" {
		t.Errorf("Amount = %q, want 1710440000000", d.Amount)
	}
	if d.Pool != "" || d.UserAddress != "" {
		t.Error("distribute should carry no pool/user")
	}
}

// TestGolden_QueueWithdrawal — Vec[i128 shares, u64 expiration]; the
// expiration lands in attributes, shares in amount.
func TestGolden_QueueWithdrawal(t *testing.T) {
	t.Parallel()
	d, err := decodeQueueWithdrawal(goldenEvent(t, "queue_withdrawal"))
	if err != nil {
		t.Fatalf("decodeQueueWithdrawal: %v", err)
	}
	if d.Amount != "730239812" {
		t.Errorf("Amount (shares) = %q, want 730239812", d.Amount)
	}
	exp, ok := d.Attributes["expiration"].(uint64)
	if !ok {
		t.Fatalf("expiration attr missing/wrong type: %v", d.Attributes["expiration"])
	}
	if exp != 1783024205 {
		t.Errorf("expiration = %d, want 1783024205", exp)
	}
}

// TestGolden_Withdraw pins bug #6 against real lake bytes (ledger
// 57072018): withdraw's body is (shares_burned, tokens_out) — the
// opposite element order from deposit's (tokens_in, shares_minted).
// Amount must be the TOKEN quantity (tokens_out) so it means the same
// thing as deposit's Amount; Amount2 carries the shares burned.
func TestGolden_Withdraw(t *testing.T) {
	t.Parallel()
	d, err := decodeWithdraw(goldenEvent(t, "withdraw"))
	if err != nil {
		t.Fatalf("decodeWithdraw: %v", err)
	}
	if d.Amount != "13030672" {
		t.Errorf("Amount (tokens_out) = %q, want 13030672", d.Amount)
	}
	if d.Amount2 != "13000000" {
		t.Errorf("Amount2 (shares_burned) = %q, want 13000000", d.Amount2)
	}
	if d.Pool == "" || d.Pool[0] != 'C' {
		t.Errorf("Pool should be a contract strkey, got %q", d.Pool)
	}
	if d.UserAddress == "" || d.UserAddress[0] != 'G' {
		t.Errorf("UserAddress should be an account strkey, got %q", d.UserAddress)
	}
}

// TestGolden_RoundTripViaDecodeOne — the full classify→decode→Event
// projection over every golden frame, exercising the consumer.go join.
func TestGolden_RoundTripViaDecodeOne(t *testing.T) {
	t.Parallel()
	for name := range goldenFrames {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ev, err := decodeOne(goldenEvent(t, name))
			if err != nil {
				t.Fatalf("decodeOne(%s): %v", name, err)
			}
			if ev.EventType != name {
				t.Errorf("EventType = %q, want %q", ev.EventType, name)
			}
			if ev.ObservedAt.IsZero() {
				t.Error("ObservedAt should be parsed from LedgerClosedAt")
			}
			if ev.Source() != SourceName {
				t.Errorf("Source() = %q, want %q", ev.Source(), SourceName)
			}
		})
	}
}

// ─── real-lake golden frames: V1/V2-divergent shapes (2026-07-09) ──
//
// These fixtures don't fit the shared goldenFrames map (V1 fixtures
// use the V1 contract; gulp_emissions/rw_zone/rw_zone_add arity
// differs by version), so they're constructed inline here, same
// base64-capture convention as goldenEvent.

// TestGolden_GulpEmissions_V1 pins bug #1 against real lake bytes
// (ledger 51524667): V1 has NO pool topic (topic_count=1) and a bare
// i128 body — not the V2 2-element Vec. Before the fix, the hard
// `len(e.Topic) < 2` check on the old single decodeGulpEmissions body
// errored EVERY one of the 209 real V1 rows.
func TestGolden_GulpEmissions_V1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID: MainnetBackstopV1,
		Ledger:     51_524_667,
		TxHash:     "438cf6d3a8f9f51f5d43bf51f533ad7ac512774eea8c3ed0ef38ce041d3725c7",
		Topic:      []string{"AAAADwAAAA5ndWxwX2VtaXNzaW9ucwAA"},
		Value:      "AAAACgAAAAAAAAAAAAABW4l4FQA=",
	}
	if got := Classify(ev); got != EventGulpEmissions {
		t.Fatalf("Classify = %q, want %q", got, EventGulpEmissions)
	}
	d, err := decodeGulpEmissions(ev)
	if err != nil {
		t.Fatalf("decodeGulpEmissions (v1): %v", err)
	}
	if d.Amount != "1492660000000" {
		t.Errorf("Amount = %q, want 1492660000000", d.Amount)
	}
	if d.Amount2 != "" {
		t.Errorf("Amount2 should be empty for the V1 bare-i128 body, got %q", d.Amount2)
	}
	if d.Pool != "" {
		t.Errorf("Pool should be NULL (not guessed) for V1 gulp_emissions, got %q", d.Pool)
	}
}

// TestGolden_GulpEmissions_V2 pins bug #5 against real lake bytes
// (ledger 56661032): topic[1] is the POOL address (matches the same
// pool topic every other event promotes), not a "token" — it belongs
// in the Pool column, not a mislabeled attribute.
func TestGolden_GulpEmissions_V2(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID: MainnetBackstopV2,
		Ledger:     56_661_032,
		TxHash:     "7027467200c6479d125dad99854411de3703e8600b765953266882ca6617cc79",
		Topic: []string{
			"AAAADwAAAA5ndWxwX2VtaXNzaW9ucwAA",
			"AAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgA==",
		},
		Value: "AAAAEAAAAAEAAAACAAAACgAAAAAAAAAAAAABHqRZ34AAAAAKAAAAAAAAAAAAAAB62LjNgA==",
	}
	if got := Classify(ev); got != EventGulpEmissions {
		t.Fatalf("Classify = %q, want %q", got, EventGulpEmissions)
	}
	d, err := decodeGulpEmissions(ev)
	if err != nil {
		t.Fatalf("decodeGulpEmissions (v2): %v", err)
	}
	if d.Amount != "1231118000000" {
		t.Errorf("Amount (new_backstop_emissions) = %q, want 1231118000000", d.Amount)
	}
	if d.Amount2 != "527622000000" {
		t.Errorf("Amount2 (new_pool_emissions) = %q, want 527622000000", d.Amount2)
	}
	if d.Pool == "" || d.Pool[0] != 'C' {
		t.Errorf("Pool should be the promoted contract strkey, got %q", d.Pool)
	}
	if _, ok := d.Attributes["token"]; ok {
		t.Error(`Attributes should no longer carry a "token" key — it's promoted to Pool now`)
	}
}

// TestGolden_RwZone_V1 pins bug #2 against real lake bytes (ledger
// 51499926): the V1 backstop's reward-zone-update topic is literally
// `rw_zone`, not `rw_zone_add` — before the fix, Classify() never
// matched it and this real event (one of 5 total) was silently
// dropped end-to-end.
func TestGolden_RwZone_V1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID: MainnetBackstopV1,
		Ledger:     51_499_926,
		TxHash:     "1b237557230a3bba3257551f54e06d5e1343d1e903b4fc505c64c18b523b5105",
		Topic:      []string{"AAAADwAAAAdyd196b25lAA=="},
		Value:      "AAAAEAAAAAEAAAACAAAAEgAAAAHrCqnY1iV5aQL6m+Y0EpHeB36N1SOnN45GpKYVLagYOwAAABIAAAAB6wqp2NYleWkC+pvmNBKR3gd+jdUjpzeORqSmFS2oGDs=",
	}
	if got := Classify(ev); got != EventRwZone {
		t.Fatalf("Classify = %q, want %q", got, EventRwZone)
	}
	d, err := decodeRwZone(ev)
	if err != nil {
		t.Fatalf("decodeRwZone: %v", err)
	}
	if d.Pool != "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP" {
		t.Errorf("Pool = %q, want CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP", d.Pool)
	}
	// This real sample's to_add and to_remove happen to be the same
	// address — a real on-chain coincidence, not a decode artifact.
	if d.Attributes["to_remove"] != "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP" {
		t.Errorf("to_remove attr = %v, want CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP", d.Attributes["to_remove"])
	}
}

// TestGolden_RwZoneAdd_V2 pins bug #3 against real lake bytes (ledger
// 56660710): the body's second element is Option<Address>, not a u32
// reward-zone index. All 5 real V2 rows carry `void` there, so the
// common case emits no `to_remove` attribute key at all.
func TestGolden_RwZoneAdd_V2(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID: MainnetBackstopV2,
		Ledger:     56_660_710,
		TxHash:     "37ddf50a6667bf5e02a67c992e41ac05312ca2d0ba35d4a6c79a18796a407ea9",
		Topic:      []string{"AAAADwAAAAtyd196b25lX2FkZAA="},
		Value:      "AAAAEAAAAAEAAAACAAAAEgAAAAESnMjMYzbx/bvcwPOYNDTDzbhf2eqFaXo3gtMY2HSlgAAAAAE=",
	}
	if got := Classify(ev); got != EventRwZoneAdd {
		t.Fatalf("Classify = %q, want %q", got, EventRwZoneAdd)
	}
	d, err := decodeRwZoneAdd(ev)
	if err != nil {
		t.Fatalf("decodeRwZoneAdd: %v", err)
	}
	if d.Pool != "CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD" {
		t.Errorf("Pool = %q, want CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD", d.Pool)
	}
	if _, ok := d.Attributes["to_remove"]; ok {
		t.Errorf("to_remove should be omitted when void, got %v", d.Attributes["to_remove"])
	}
	if _, ok := d.Attributes["index"]; ok {
		t.Error(`Attributes should no longer carry a spurious "index" key`)
	}
}

// TestDecodeRwZoneRemove_SyntheticFromSource covers bug #4.
// rw_zone_remove has ZERO lake occurrences as of 2026-07-09 (never
// fired on mainnet) so this is SYNTHETIC-FROM-SOURCE, not a real-lake
// pin: the shape (topics=[sym]; data=bare Address) is taken from
// blend-contracts-v2 backstop/src/events.rs's actual `let topics =
// (...)` / `publish()` call, which disagrees with that function's own
// doc comment (see decode.go's decodeRwZoneRemove doc for the
// discrepancy). Unverified against real bytes.
func TestDecodeRwZoneRemove_SyntheticFromSource(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0xBB)
	e := &events.Event{
		Topic: []string{TopicSymbolRwZoneRemove},
		Value: b64SV(t, contractAddrSV(t, pool)),
	}
	if got := Classify(e); got != EventRwZoneRemove {
		t.Fatalf("Classify = %q, want %q", got, EventRwZoneRemove)
	}
	d, err := decodeRwZoneRemove(e)
	if err != nil {
		t.Fatalf("decodeRwZoneRemove: %v", err)
	}
	if d.Pool != pool {
		t.Errorf("Pool = %q, want %q", d.Pool, pool)
	}
}

// ─── synthetic-SCVal helpers (for the negative / edge cases the
//     golden frames don't cover) ────────────────────────────────

func symbolSV(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func i128SV(n *big.Int) xdr.ScVal {
	hi, lo := splitBigInt128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

func voidSV() xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvVoid}
}

func vecSV(vals ...xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(vals)
	pv := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &pv}
}

func contractStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func accountStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func contractAddrSV(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	var cid xdr.ContractId
	raw, err := strkey.Decode(strkey.VersionByteContract, strk)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	copy(cid[:], raw)
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func accountAddrSV(t *testing.T, strk string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, strk)
	if err != nil {
		t.Fatalf("strkey.Decode: %v", err)
	}
	var ed xdr.Uint256
	copy(ed[:], raw)
	acc := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &ed}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &acc}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func b64SV(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func splitBigInt128(n *big.Int) (hi int64, lo uint64) {
	twoTo64 := new(big.Int).Lsh(big.NewInt(1), 64)
	mask64 := new(big.Int).Sub(twoTo64, big.NewInt(1))
	if n.Sign() >= 0 {
		loBig := new(big.Int).And(n, mask64)
		hiBig := new(big.Int).Rsh(n, 64)
		return hiBig.Int64(), loBig.Uint64()
	}
	twoTo128 := new(big.Int).Lsh(big.NewInt(1), 128)
	u := new(big.Int).Add(twoTo128, n)
	loBig := new(big.Int).And(u, mask64)
	hiBig := new(big.Int).Rsh(u, 64)
	return int64(hiBig.Uint64()), loBig.Uint64()
}

// ─── Classify ────────────────────────────────────────────────────

func TestClassify_AllEventTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		topic    string
		expected string
	}{
		{TopicSymbolDeposit, EventDeposit},
		{TopicSymbolClaim, EventClaim},
		{TopicSymbolDonate, EventDonate},
		{TopicSymbolQueueWithdrawal, EventQueueWithdrawal},
		{TopicSymbolWithdraw, EventWithdraw},
		{TopicSymbolDistribute, EventDistribute},
		{TopicSymbolGulpEmissions, EventGulpEmissions},
		{TopicSymbolDequeueWithdrawal, EventDequeueWithdrawal},
		{TopicSymbolDraw, EventDraw},
		{TopicSymbolRwZoneAdd, EventRwZoneAdd},
		{TopicSymbolRwZone, EventRwZone},
		{TopicSymbolRwZoneRemove, EventRwZoneRemove},
	}
	for _, c := range cases {
		c := c
		t.Run(c.expected, func(t *testing.T) {
			t.Parallel()
			e := &events.Event{Topic: []string{c.topic}}
			if got := Classify(e); got != c.expected {
				t.Errorf("Classify(%s) = %q, want %q", c.expected, got, c.expected)
			}
		})
	}
}

func TestClassify_UnknownAndEmpty(t *testing.T) {
	t.Parallel()
	if got := Classify(&events.Event{Topic: []string{b64SV(t, symbolSV("transfer"))}}); got != "" {
		t.Errorf("unknown topic classified as %q", got)
	}
	if got := Classify(&events.Event{Topic: nil}); got != "" {
		t.Errorf("empty topic classified as %q", got)
	}
}

// ─── synthetic decode coverage for the kinds the golden frames don't
//     carry (withdraw / gulp_emissions / dequeue / draw / rw_zone_add)

func TestDecodeWithdraw_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x11)
	user := accountStrkey(t, 0x22)
	// Body order is (shares_burned, tokens_out) per
	// blend-contracts-v2's actual event — 900 shares burned, 450
	// tokens paid out.
	body := b64SV(t, vecSV(i128SV(big.NewInt(900)), i128SV(big.NewInt(450))))
	e := &events.Event{
		Topic: []string{
			TopicSymbolWithdraw,
			b64SV(t, contractAddrSV(t, pool)),
			b64SV(t, accountAddrSV(t, user)),
		},
		Value: body,
	}
	d, err := decodeWithdraw(e)
	if err != nil {
		t.Fatalf("decodeWithdraw: %v", err)
	}
	// Amount is normalized to the TOKEN quantity (tokens_out); Amount2
	// carries the shares burned — the opposite of the pre-fix mapping.
	if d.Amount != "450" || d.Amount2 != "900" {
		t.Errorf("Amount/Amount2 = %q/%q, want 450/900 (tokens_out/shares_burned)", d.Amount, d.Amount2)
	}
	if d.Pool != pool || d.UserAddress != user {
		t.Errorf("pool/user mismatch: %q / %q", d.Pool, d.UserAddress)
	}
}

func TestDecodeGulpEmissions_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x33)
	body := b64SV(t, vecSV(i128SV(big.NewInt(7)), i128SV(big.NewInt(8))))
	e := &events.Event{
		Topic: []string{TopicSymbolGulpEmissions, b64SV(t, contractAddrSV(t, pool))},
		Value: body,
	}
	d, err := decodeGulpEmissions(e)
	if err != nil {
		t.Fatalf("decodeGulpEmissions: %v", err)
	}
	if d.Amount != "7" || d.Amount2 != "8" {
		t.Errorf("Amount/Amount2 = %q/%q, want 7/8", d.Amount, d.Amount2)
	}
	// topic[1] is the pool address — promoted to Pool, not stashed as
	// a "token" attribute (bug #5).
	if d.Pool != pool {
		t.Errorf("Pool = %q, want %q", d.Pool, pool)
	}
	if _, ok := d.Attributes["token"]; ok {
		t.Error(`Attributes should not carry a "token" key`)
	}
}

// TestDecodeGulpEmissions_V1Synthetic exercises the V1 branch (no
// pool topic, bare i128 body) with a hand-built event — the
// real-lake pin lives in TestGolden_GulpEmissions_V1.
func TestDecodeGulpEmissions_V1Synthetic(t *testing.T) {
	t.Parallel()
	e := &events.Event{
		Topic: []string{TopicSymbolGulpEmissions},
		Value: b64SV(t, i128SV(big.NewInt(42))),
	}
	d, err := decodeGulpEmissions(e)
	if err != nil {
		t.Fatalf("decodeGulpEmissions (v1): %v", err)
	}
	if d.Amount != "42" {
		t.Errorf("Amount = %q, want 42", d.Amount)
	}
	if d.Amount2 != "" || d.Pool != "" {
		t.Errorf("Amount2/Pool should be empty for the V1 shape, got %q/%q", d.Amount2, d.Pool)
	}
}

func TestDecodeDraw_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x44)
	to := contractStrkey(t, 0x55)
	body := b64SV(t, vecSV(contractAddrSV(t, to), i128SV(big.NewInt(123))))
	e := &events.Event{
		Topic: []string{TopicSymbolDraw, b64SV(t, contractAddrSV(t, pool))},
		Value: body,
	}
	d, err := decodeDraw(e)
	if err != nil {
		t.Fatalf("decodeDraw: %v", err)
	}
	if d.Amount != "123" {
		t.Errorf("Amount = %q, want 123", d.Amount)
	}
	if d.Pool != pool {
		t.Errorf("Pool = %q, want %q", d.Pool, pool)
	}
	if d.Attributes["to"] != to {
		t.Errorf("to attr = %v, want %q", d.Attributes["to"], to)
	}
}

func TestDecodeDequeueWithdrawal_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x66)
	user := accountStrkey(t, 0x77)
	e := &events.Event{
		Topic: []string{
			TopicSymbolDequeueWithdrawal,
			b64SV(t, contractAddrSV(t, pool)),
			b64SV(t, accountAddrSV(t, user)),
		},
		Value: b64SV(t, i128SV(big.NewInt(555))),
	}
	d, err := decodeDequeueWithdrawal(e)
	if err != nil {
		t.Fatalf("decodeDequeueWithdrawal: %v", err)
	}
	if d.Amount != "555" || d.Pool != pool || d.UserAddress != user {
		t.Errorf("got amount=%q pool=%q user=%q", d.Amount, d.Pool, d.UserAddress)
	}
}

// TestDecodeRwZoneAdd_Synthetic covers the to_remove=Some(Address)
// branch — every real lake row observed to_remove=void (see
// TestGolden_RwZoneAdd_V2), so this hand-built case is the only
// coverage for the "actually removing a pool" path.
func TestDecodeRwZoneAdd_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x88)
	toRemove := contractStrkey(t, 0x89)
	body := b64SV(t, vecSV(contractAddrSV(t, pool), contractAddrSV(t, toRemove)))
	e := &events.Event{Topic: []string{TopicSymbolRwZoneAdd}, Value: body}
	d, err := decodeRwZoneAdd(e)
	if err != nil {
		t.Fatalf("decodeRwZoneAdd: %v", err)
	}
	if d.Pool != pool {
		t.Errorf("Pool = %q, want %q", d.Pool, pool)
	}
	if d.Attributes["to_remove"] != toRemove {
		t.Errorf("to_remove attr = %v, want %q", d.Attributes["to_remove"], toRemove)
	}
}

// TestDecodeRwZoneAdd_VoidToRemove covers the to_remove=None branch
// with a synthetic Void SCVal — the shape every real lake row uses.
func TestDecodeRwZoneAdd_VoidToRemove(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x8A)
	body := b64SV(t, vecSV(contractAddrSV(t, pool), voidSV()))
	e := &events.Event{Topic: []string{TopicSymbolRwZoneAdd}, Value: body}
	d, err := decodeRwZoneAdd(e)
	if err != nil {
		t.Fatalf("decodeRwZoneAdd: %v", err)
	}
	if d.Pool != pool {
		t.Errorf("Pool = %q, want %q", d.Pool, pool)
	}
	if _, ok := d.Attributes["to_remove"]; ok {
		t.Errorf("to_remove should be omitted when void, got %v", d.Attributes["to_remove"])
	}
}

func TestDecodeDonate_Synthetic(t *testing.T) {
	t.Parallel()
	pool := contractStrkey(t, 0x99)
	from := contractStrkey(t, 0xAA)
	e := &events.Event{
		Topic: []string{
			TopicSymbolDonate,
			b64SV(t, contractAddrSV(t, pool)),
			b64SV(t, contractAddrSV(t, from)),
		},
		Value: b64SV(t, i128SV(big.NewInt(42))),
	}
	d, err := decodeDonate(e)
	if err != nil {
		t.Fatalf("decodeDonate: %v", err)
	}
	if d.Amount != "42" || d.Pool != pool {
		t.Errorf("amount/pool = %q/%q", d.Amount, d.Pool)
	}
	if d.Attributes["from"] != from {
		t.Errorf("from attr = %v, want %q", d.Attributes["from"], from)
	}
}

// ─── ADR-0003 large-i128 guard ───────────────────────────────────

func TestDecode_LargeI128_NoTruncation(t *testing.T) {
	t.Parallel()
	big1 := new(big.Int)
	big1.SetString("999999999999999999999999999999", 10) // >> 2^53
	e := &events.Event{
		Topic: []string{TopicSymbolDistribute},
		Value: b64SV(t, i128SV(big1)),
	}
	d, err := decodeDistribute(e)
	if err != nil {
		t.Fatalf("decodeDistribute: %v", err)
	}
	if d.Amount != big1.String() {
		t.Errorf("large i128 lost precision: got %q, want %q", d.Amount, big1.String())
	}
}

// ─── malformed-event guards ──────────────────────────────────────

func TestDecode_ShortTopic(t *testing.T) {
	t.Parallel()
	if _, err := decodeDeposit(&events.Event{Topic: []string{TopicSymbolDeposit}}); !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("deposit short-topic: want ErrMalformedTopic, got %v", err)
	}
	if _, err := decodeClaim(&events.Event{Topic: []string{TopicSymbolClaim}}); !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("claim short-topic: want ErrMalformedTopic, got %v", err)
	}
}

func TestDecode_MalformedBody(t *testing.T) {
	t.Parallel()
	// deposit body must be a 2-Vec; hand it a bare i128.
	pool := contractStrkey(t, 0x11)
	user := accountStrkey(t, 0x22)
	e := &events.Event{
		Topic: []string{
			TopicSymbolDeposit,
			b64SV(t, contractAddrSV(t, pool)),
			b64SV(t, accountAddrSV(t, user)),
		},
		Value: b64SV(t, i128SV(big.NewInt(1))),
	}
	if _, err := decodeDeposit(e); !errors.Is(err, ErrMalformedBody) {
		t.Errorf("deposit malformed-body: want ErrMalformedBody, got %v", err)
	}
}

// ─── topic-symbol encoding stability ─────────────────────────────

func TestTopicSymbol_StableEncoding(t *testing.T) {
	t.Parallel()
	cases := []struct {
		got  string
		want xdr.ScVal
	}{
		{TopicSymbolDeposit, symbolSV(EventDeposit)},
		{TopicSymbolClaim, symbolSV(EventClaim)},
		{TopicSymbolDonate, symbolSV(EventDonate)},
		{TopicSymbolQueueWithdrawal, symbolSV(EventQueueWithdrawal)},
		{TopicSymbolWithdraw, symbolSV(EventWithdraw)},
		{TopicSymbolDistribute, symbolSV(EventDistribute)},
		{TopicSymbolGulpEmissions, symbolSV(EventGulpEmissions)},
		{TopicSymbolDequeueWithdrawal, symbolSV(EventDequeueWithdrawal)},
		{TopicSymbolDraw, symbolSV(EventDraw)},
		{TopicSymbolRwZoneAdd, symbolSV(EventRwZoneAdd)},
		{TopicSymbolRwZone, symbolSV(EventRwZone)},
		{TopicSymbolRwZoneRemove, symbolSV(EventRwZoneRemove)},
	}
	for _, c := range cases {
		c := c
		if c.got != b64SV(t, c.want) {
			t.Errorf("symbol drift: pkg = %q, re-encoded = %q", c.got, b64SV(t, c.want))
		}
	}
}
