package comet

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── Fixture helpers ─────────────────────────────────────────────

// contractStrkeyFromSeed produces a valid C-strkey from a 32-byte
// seed so every fixture passes strkey checksum without depending on
// real mainnet addresses we'd then need to pin.
func contractStrkeyFromSeed(t *testing.T, tag byte) string {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = tag ^ byte(i)
	}
	s, err := strkey.Encode(strkey.VersionByteContract, seed)
	if err != nil {
		t.Fatalf("strkey encode: %v", err)
	}
	return s
}

func accountStrkeyFromSeed(t *testing.T, tag byte) string {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = tag | byte(i)
	}
	s, err := strkey.Encode(strkey.VersionByteAccountID, seed)
	if err != nil {
		t.Fatalf("strkey encode: %v", err)
	}
	return s
}

// addressScValFromStrkey turns a G-strkey / C-strkey into an
// ScVal::Address.
func addressScValFromStrkey(t *testing.T, s string) xdr.ScVal {
	t.Helper()
	if len(s) == 0 {
		t.Fatal("empty strkey")
	}
	switch s[0] {
	case 'G':
		raw, err := strkey.Decode(strkey.VersionByteAccountID, s)
		if err != nil {
			t.Fatalf("decode G: %v", err)
		}
		var pub xdr.Uint256
		copy(pub[:], raw)
		aid := xdr.AccountId{
			Type:    xdr.PublicKeyTypePublicKeyTypeEd25519,
			Ed25519: &pub,
		}
		addr := xdr.ScAddress{
			Type:      xdr.ScAddressTypeScAddressTypeAccount,
			AccountId: &aid,
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	case 'C':
		raw, err := strkey.Decode(strkey.VersionByteContract, s)
		if err != nil {
			t.Fatalf("decode C: %v", err)
		}
		var cid xdr.ContractId
		copy(cid[:], raw)
		addr := xdr.ScAddress{
			Type:       xdr.ScAddressTypeScAddressTypeContract,
			ContractId: &cid,
		}
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &addr}
	default:
		t.Fatalf("unknown strkey prefix: %s", s)
		return xdr.ScVal{}
	}
}

// i128ScVal splits a signed *big.Int into the Hi/Lo parts.
func i128ScVal(t *testing.T, n *big.Int) xdr.ScVal {
	t.Helper()
	hi, lo := splitBigInt128(n)
	p := xdr.Int128Parts{Hi: xdr.Int64(hi), Lo: xdr.Uint64(lo)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
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

// encodeSwapBody assembles a (caller, token_in, token_out,
// token_amount_in, token_amount_out) Map body and returns its base64
// SCVal blob.
func encodeSwapBody(t *testing.T, caller, tokenIn, tokenOut string, amountIn, amountOut *big.Int) string {
	t.Helper()
	callerSv := addressScValFromStrkey(t, caller)
	tokenInSv := addressScValFromStrkey(t, tokenIn)
	tokenOutSv := addressScValFromStrkey(t, tokenOut)
	amountInSv := i128ScVal(t, amountIn)
	amountOutSv := i128ScVal(t, amountOut)

	keys := []string{"caller", "token_amount_in", "token_amount_out", "token_in", "token_out"}
	vals := []xdr.ScVal{callerSv, amountInSv, amountOutSv, tokenInSv, tokenOutSv}
	m := make(xdr.ScMap, len(keys))
	for i, k := range keys {
		sym := xdr.ScSymbol(k)
		m[i] = xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: vals[i],
		}
	}
	pm := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
	b, err := body.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

// ─── Tests ───────────────────────────────────────────────────────

func TestClassify_MatchesPoolSwap(t *testing.T) {
	e := &events.Event{Topic: []string{TopicSymbolPool, TopicSymbolSwap}}
	if !classifySwap(e) {
		t.Errorf("expected classify true")
	}
	if classifySwap(&events.Event{Topic: []string{TopicSymbolPool}}) {
		t.Errorf("expected false for single-topic event")
	}
	if classifySwap(&events.Event{Topic: []string{TopicSymbolSwap, TopicSymbolPool}}) {
		t.Errorf("expected false for swapped-order topics")
	}
}

func TestDecodeSwap_HappyPath(t *testing.T) {
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	amountIn := big.NewInt(1_000_000_000)  // 1.0 at 9 decimals
	amountOut := big.NewInt(3_500_000_000) // 3.5 at 9 decimals

	body := encodeSwapBody(t, caller, tokenIn, tokenOut, amountIn, amountOut)
	ev := &events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         52_000_000,
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	closedAt, _ := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	trade, err := decodeSwap(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeSwap: %v", err)
	}
	if trade.Source != SourceName {
		t.Errorf("Source = %q want %q", trade.Source, SourceName)
	}
	if trade.BaseAmount.BigInt().Cmp(amountIn) != 0 {
		t.Errorf("BaseAmount = %s want %s", trade.BaseAmount, amountIn)
	}
	if trade.QuoteAmount.BigInt().Cmp(amountOut) != 0 {
		t.Errorf("QuoteAmount = %s want %s", trade.QuoteAmount, amountOut)
	}
	if trade.Taker != caller {
		t.Errorf("Taker = %q want %q", trade.Taker, caller)
	}
	// Pair base == token_in, quote == token_out.
	wantBase, _ := canonical.NewSorobanAsset(tokenIn)
	if !trade.Pair.Base.Equal(wantBase) {
		t.Errorf("Pair.Base = %+v want %+v", trade.Pair.Base, wantBase)
	}
}

func TestDecodeSwap_NonPositiveAmounts_Rejects(t *testing.T) {
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut, big.NewInt(0), big.NewInt(5))
	ev := &events.Event{
		Topic: []string{TopicSymbolPool, TopicSymbolSwap},
		Value: body,
	}
	_, err := decodeSwap(ev, time.Now())
	if !errors.Is(err, ErrNonPositiveAmounts) {
		t.Errorf("expected ErrNonPositiveAmounts, got %v", err)
	}
}

func TestDecodeSwap_WrongTopic_Rejects(t *testing.T) {
	ev := &events.Event{Topic: []string{TopicSymbolPool, TopicSymbolPool}}
	_, err := decodeSwap(ev, time.Now())
	if !errors.Is(err, ErrNotCometSwap) {
		t.Errorf("expected ErrNotCometSwap, got %v", err)
	}
}

// TestDecodeSwap_DispatchIsByTopicNotContract pins the AGENTS.md
// surprise: the Comet decoder matches by (POOL, swap) topic shape,
// not by contract address. Any pubnet contract that deploys
// Balancer-v1 Comet bytecode emits the same wire shape and is
// attributed to Source="comet".
//
// This is intentional — the decoder has no concept of "the one
// canonical Comet pool"; it's a generic Balancer-v1 decoder.
// Operators who want narrower coverage filter downstream by
// (Trade.Source, contract_id) rather than at dispatch time.
//
// The guard exists to catch a future change that would
// inadvertently narrow the decoder to a specific contract
// allow-list — that would silently drop legitimate trades from
// any new Balancer-v1 deployment. F-1242 (audit-2026-05-12).
func TestDecodeSwap_DispatchIsByTopicNotContract(t *testing.T) {
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut, big.NewInt(1_000), big.NewInt(2_000))

	// Same body, two distinct contract IDs (the known Blend
	// backstop pool vs an arbitrary "future" Balancer-v1
	// deployment). The decoder doesn't read ContractID, so both
	// events MUST attribute to Source="comet".
	cometContractA := contractStrkeyFromSeed(t, 0xAA)
	cometContractB := contractStrkeyFromSeed(t, 0xBB)

	closedAt := time.Now()
	for _, contractID := range []string{cometContractA, cometContractB} {
		ev := &events.Event{
			ContractID: contractID,
			Topic:      []string{TopicSymbolPool, TopicSymbolSwap},
			Value:      body,
		}
		trade, err := decodeSwap(ev, closedAt)
		if err != nil {
			t.Fatalf("decodeSwap from contract %s: %v", contractID, err)
		}
		if trade.Source != SourceName {
			t.Errorf("contract %s: Source = %q, want %q (dispatch is by topic, not contract)",
				contractID, trade.Source, SourceName)
		}
	}
}

func TestDecodeSwap_MissingBodyField_Malformed(t *testing.T) {
	// Build a map missing token_out.
	caller := accountStrkeyFromSeed(t, 0x10)
	callerSv := addressScValFromStrkey(t, caller)
	amountInSv := i128ScVal(t, big.NewInt(1))
	amountOutSv := i128ScVal(t, big.NewInt(2))
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenInSv := addressScValFromStrkey(t, tokenIn)

	keys := []string{"caller", "token_amount_in", "token_amount_out", "token_in"}
	vals := []xdr.ScVal{callerSv, amountInSv, amountOutSv, tokenInSv}
	m := make(xdr.ScMap, len(keys))
	for i, k := range keys {
		sym := xdr.ScSymbol(k)
		m[i] = xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: vals[i],
		}
	}
	pm := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &pm}
	b, _ := body.MarshalBinary()
	bodyB64 := base64.StdEncoding.EncodeToString(b)

	ev := &events.Event{
		Topic: []string{TopicSymbolPool, TopicSymbolSwap},
		Value: bodyB64,
	}
	_, err := decodeSwap(ev, time.Now())
	if !errors.Is(err, ErrMalformedPayload) {
		t.Errorf("expected ErrMalformedPayload, got %v", err)
	}
}

// ─── Real mainnet bytes (GH-932) ─────────────────────────────────

// cometSwapFixture mirrors internal/events.Event's wire shape, so a
// fixture file unmarshals straight into one. Captured via getEvents
// against the curated allowlisted pool — see test/fixtures/comet/README.md.
type cometSwapFixture struct {
	ContractID     string   `json:"contract_id"`
	WasmHash       string   `json:"wasm_hash"`
	Ledger         uint32   `json:"ledger"`
	TxHash         string   `json:"tx_hash"`
	OpIndex        int      `json:"op_index"`
	LedgerClosedAt string   `json:"ledger_closed_at"`
	Topics         []string `json:"topics"`
	Value          string   `json:"value"`
}

// TestRealMainnetFixtures_comet replays a real captured Comet POOL
// swap event through the full dispatcher-facing path (Matches +
// Decode) — unlike every other test in this file, which synthesises
// its event bytes from a seeded strkey. Proves the event schema this
// package documents (events.go's SwapEvent Map shape) against a byte
// the chain actually produced, not just against upstream Rust source.
func TestRealMainnetFixtures_comet(t *testing.T) {
	root := filepath.Join("..", "..", "..", "test", "fixtures", "comet")
	dirs, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("fixtures root unreadable: %v", err)
	}
	total := 0
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		dir := filepath.Join(root, d.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range files {
			if filepath.Ext(f.Name()) != ".json" {
				continue
			}
			total++
			t.Run(d.Name()+"/"+f.Name(), func(t *testing.T) {
				runCometRealFixture(t, filepath.Join(dir, f.Name()))
			})
		}
	}
	if total == 0 {
		t.Skip("no fixtures present")
	}
}

func runCometRealFixture(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var fx cometSwapFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	closedAt, err := time.Parse(time.RFC3339, fx.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse ledger_closed_at: %v", err)
	}

	ev := events.Event{
		Ledger:         fx.Ledger,
		ContractID:     fx.ContractID,
		OperationIndex: fx.OpIndex,
		TxHash:         fx.TxHash,
		Topic:          fx.Topics,
		Value:          fx.Value,
		LedgerClosedAt: fx.LedgerClosedAt,
	}

	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatal("Matches() = false for a real event from the curated allowlisted pool")
	}
	outs, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(outs) != 1 {
		t.Fatalf("got %d events, want 1", len(outs))
	}
	tradeEv, ok := outs[0].(TradeEvent)
	if !ok {
		t.Fatalf("output is %T, want TradeEvent", outs[0])
	}
	tr := tradeEv.Trade

	if tr.Ledger != fx.Ledger || tr.TxHash != fx.TxHash {
		t.Errorf("ledger/txhash = %d/%s, want %d/%s", tr.Ledger, tr.TxHash, fx.Ledger, fx.TxHash)
	}
	if !tr.Timestamp.Equal(closedAt) {
		t.Errorf("Timestamp = %v, want %v", tr.Timestamp, closedAt)
	}
	if tr.BaseAmount.Sign() <= 0 || tr.QuoteAmount.Sign() <= 0 {
		t.Errorf("amounts not positive: base=%s quote=%s", tr.BaseAmount, tr.QuoteAmount)
	}
	if tr.Pair.Base.Equal(tr.Pair.Quote) {
		t.Error("Pair.Base == Pair.Quote on a real capture — should be an ordinary (non-self-pair) swap")
	}
	if tr.Taker == "" {
		t.Error("Taker address empty")
	}
	// Sanity: i128 amounts shouldn't be > 2^100 for a real token.
	maxSane := new(big.Int).Lsh(big.NewInt(1), 100)
	if tr.BaseAmount.BigInt().Cmp(maxSane) > 0 || tr.QuoteAmount.BigInt().Cmp(maxSane) > 0 {
		t.Errorf("amount unreasonably large — i128 misalign? base=%s quote=%s", tr.BaseAmount, tr.QuoteAmount)
	}
	if err := tr.Validate(); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}
