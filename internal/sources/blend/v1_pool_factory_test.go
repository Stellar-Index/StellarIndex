package blend

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── real-lake golden frames (base64 XDR) ────────────────────────
//
// Captured via a read-only ClickHouse query against the
// r1 raw lake (stellar.contract_events), scoped by topic_0_sym +
// ledger_seq — see internal/sources/blend/README.md "Known gap" for
// the evidence trail. These PIN the V1 pool-factory's simpler
// vocabulary: if a decode helper drifts, the asserted fields change.

// TestGolden_UpdateEmissionsV1 pins decodeUpdateEmissions against a
// real V1 pool event: ledger 51,524,668, pool CDVQVKOY…, tx
// f56fabf7…, event_index 4. Topic: [Symbol("update_emissions")].
// Body: bare i128 = 447798000000.
func TestGolden_UpdateEmissionsV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP",
		Ledger:         51_524_668,
		LedgerClosedAt: "2026-04-14T00:00:00Z",
		TxHash:         "f56fabf75569b7106703ad0b6d26eb565d63a66d5e99a9295a180962fb3f9945",
		OperationIndex: 0,
		EventIndex:     4,
		Topic:          []string{"AAAADwAAABB1cGRhdGVfZW1pc3Npb25z"},
		Value:          "AAAACgAAAAAAAAAAAAAAaELXOYA=",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeUpdateEmissions(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeUpdateEmissions: %v", err)
	}
	if out.Pool != ev.ContractID {
		t.Errorf("Pool=%q want %q", out.Pool, ev.ContractID)
	}
	if out.Kind != EventUpdateEmissions {
		t.Errorf("Kind=%q want %q", out.Kind, EventUpdateEmissions)
	}
	want := big.NewInt(447798000000)
	if out.Amount == nil || out.Amount.Cmp(want) != 0 {
		t.Errorf("Amount=%v want %s", out.Amount, want)
	}
}

// TestGolden_NewLiquidationAuctionV1 pins decodeNewLiquidationAuctionV1
// against a real V1 pool event: ledger 51,611,821, pool CDVQVKOY…, tx
// c128f9ce…. Topic: [Symbol("new_liquidation_auction"),
// Address(user)]. Body: Map{bid, block, lot} — the SAME AuctionData
// shape decodeAuctionData already parses for V2's new_auction, but
// with no auction_type topic and no percent field.
func TestGolden_NewLiquidationAuctionV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP",
		Ledger:         51_611_821,
		LedgerClosedAt: "2026-04-16T00:00:00Z",
		TxHash:         "c128f9ce868563035060163b5acabe5115da687e4aaaca438e6ee86f3fbdabb1",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			"AAAADwAAABduZXdfbGlxdWlkYXRpb25fYXVjdGlvbgA=",
			"AAAAEgAAAAAAAAAA55lmZg3eGvDC+CmG2pYQJ98JuJKj9DkZw0DnosK1jsk=",
		},
		Value: "AAAAEQAAAAEAAAADAAAADwAAAANiaWQAAAAAEQAAAAEAAAABAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAAAoAAAAAAAAAAAAAAABAX+1PAAAADwAAAAVibG9jawAAAAAAAAMDE4iuAAAADwAAAANsb3QAAAAAEQAAAAEAAAABAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAAoAAAAAAAAAAAAAAAK2pAaD",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeNewLiquidationAuctionV1(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeNewLiquidationAuctionV1: %v", err)
	}
	if out.ContractID != ev.ContractID {
		t.Errorf("ContractID=%q want %q", out.ContractID, ev.ContractID)
	}
	if out.Kind != EventNewLiquidationAuction {
		t.Errorf("Kind=%q want %q", out.Kind, EventNewLiquidationAuction)
	}
	wantUser := "GDTZSZTGBXPBV4GC7AUYNWUWCAT56CNYSKR7IOIZYNAOPIWCWWHMTIPS"
	if out.Target != wantUser {
		t.Errorf("Target=%q want %q", out.Target, wantUser)
	}
	if out.AuctionBlock != 51_611_822 {
		t.Errorf("AuctionBlock=%d want 51611822", out.AuctionBlock)
	}
	if len(out.AuctionBid) != 1 {
		t.Fatalf("AuctionBid len=%d want 1", len(out.AuctionBid))
	}
	if want := "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"; out.AuctionBid[0].Asset != want {
		t.Errorf("AuctionBid[0].Asset=%q want %q", out.AuctionBid[0].Asset, want)
	}
	if want := big.NewInt(1_080_028_495); out.AuctionBid[0].Amount.Cmp(want) != 0 {
		t.Errorf("AuctionBid[0].Amount=%s want %s", out.AuctionBid[0].Amount, want)
	}
	if len(out.AuctionLot) != 1 {
		t.Fatalf("AuctionLot len=%d want 1", len(out.AuctionLot))
	}
	if want := "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"; out.AuctionLot[0].Asset != want {
		t.Errorf("AuctionLot[0].Asset=%q want %q", out.AuctionLot[0].Asset, want)
	}
	if want := big.NewInt(11_654_137_475); out.AuctionLot[0].Amount.Cmp(want) != 0 {
		t.Errorf("AuctionLot[0].Amount=%s want %s", out.AuctionLot[0].Amount, want)
	}
}

// TestGolden_DeleteLiquidationAuctionV1 pins
// decodeDeleteLiquidationAuctionV1 against the single real V1
// occurrence: ledger 54,890,906, pool CBP7NO6F…, tx 24fb3d09…. Topic:
// [Symbol("delete_liquidation_auction"), Address(user)]. Body:
// ScvVoid (not parsed — same convention as V2's delete_auction).
func TestGolden_DeleteLiquidationAuctionV1(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:           "contract",
		ContractID:     "CBP7NO6F7FRDHSOFQBT2L2UWYIZ2PU76JKVRYAQTG3KZSQLYAOKIF2WB",
		Ledger:         54_890_906,
		LedgerClosedAt: "2026-06-08T00:00:00Z",
		TxHash:         "24fb3d09f926c1ffe0159e76befe2a34eb1516843347af400c8a9ea4f3a69e91",
		OperationIndex: 0,
		EventIndex:     0,
		Topic: []string{
			"AAAADwAAABpkZWxldGVfbGlxdWlkYXRpb25fYXVjdGlvbgAA",
			"AAAAEgAAAAAAAAAAX0K546vMzvmIdxWJNrpvLrwHILr9Bg/9TY3zC6vwGlk=",
		},
		Value: "AAAAAQ==",
	}
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatalf("parse closedAt: %v", err)
	}
	out, err := decodeDeleteLiquidationAuctionV1(ev, closedAt)
	if err != nil {
		t.Fatalf("decodeDeleteLiquidationAuctionV1: %v", err)
	}
	if out.ContractID != ev.ContractID {
		t.Errorf("ContractID=%q want %q", out.ContractID, ev.ContractID)
	}
	if out.Kind != EventDeleteLiquidationAuction {
		t.Errorf("Kind=%q want %q", out.Kind, EventDeleteLiquidationAuction)
	}
	wantUser := "GBPUFOPDVPGM56MIO4KYSNV2N4XLYBZAXL6QMD75JWG7GC5L6ANFSYBL"
	if out.Target != wantUser {
		t.Errorf("Target=%q want %q", out.Target, wantUser)
	}
}

// TestDecodeUpdateEmissionsV1_TopicArityMismatch pins the fail-loud
// path — a stray extra topic must error rather than silently decode.
func TestDecodeUpdateEmissionsV1_TopicArityMismatch(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{
			"AAAADwAAABB1cGRhdGVfZW1pc3Npb25z",
			"AAAAEgAAAAAAAAAA55lmZg3eGvDC+CmG2pYQJ98JuJKj9DkZw0DnosK1jsk=", // unexpected extra topic
		},
		Value: "AAAACgAAAAAAAAAAAAAAaELXOYA=",
	}
	if _, err := decodeUpdateEmissions(ev, time.Now()); err == nil {
		t.Fatal("expected ErrMalformedPayload for arity mismatch")
	}
}

// TestGolden_DeployV1Factory pins decodeDeploy against the V1 factory's
// first real deploy: ledger 51,499,915, tx 951cea40…, which deploys pool
// CDVQVKOY…. Topic: [Symbol("deploy")]. Body: Address(pool).
func TestGolden_DeployV1Factory(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Type:       "contract",
		ContractID: MainnetPoolFactoryV1,
		Ledger:     51_499_915,
		TxHash:     "951cea4049cad28cb5a7194c6752b946d2b6362335f96a1d919b69da6f25cd9f",
		Topic:      []string{"AAAADwAAAAZkZXBsb3kAAA=="},
		Value:      "AAAAEgAAAAHrCqnY1iV5aQL6m+Y0EpHeB36N1SOnN45GpKYVLagYOw==",
	}
	out, err := decodeDeploy(ev, time.Unix(0, 0).UTC())
	if err != nil {
		t.Fatalf("decodeDeploy: %v", err)
	}
	if out.Kind != EventDeploy {
		t.Errorf("Kind=%q want %q", out.Kind, EventDeploy)
	}
	if want := "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"; out.Target != want {
		t.Errorf("Target=%q want %q", out.Target, want)
	}
	if out.ContractID != MainnetPoolFactoryV1 {
		t.Errorf("ContractID=%q want %q", out.ContractID, MainnetPoolFactoryV1)
	}
}

// Ledger 52,504,175: [fill_auction, Address(user), u32(2)], body
// (filler, i128 100). User is the V1 backstop (Interest auction).
func TestGolden_FillAuctionV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger   uint32
		pool     string
		typ      uint32
		user     string
		filler   string
		pct      int64
		evIndex  uint32
		txPrefix string
	}{
		{51_612_222, v1PoolCDVQ, AuctionTypeUserLiquidation, "GDTZSZTGBXPBV4GC7AUYNWUWCAT56CNYSKR7IOIZYNAOPIWCWWHMTIPS", "GASND6BBFGDGWDLP2DJCFDAKL7GHHZAYQ6PENFHSHMLEMFKAVLZCDQXJ", 100, 0, "2a55240c"},
		{52_430_677, v1PoolCDVQ, AuctionTypeBadDebt, MainnetBackstopV1, "GAWL2CH2JH5APO5GJZ5OH6SILW4XUFW7Z72FZDAAUOLK5QZTS363QDDC", 100, 2, "868b7762"},
		{52_504_175, v1PoolCDVQ, AuctionTypeInterest, MainnetBackstopV1, "GAWL2CH2JH5APO5GJZ5OH6SILW4XUFW7Z72FZDAAUOLK5QZTS363QDDC", 100, 4, "61864915"},
		{55_458_184, "CAQF5KNOFIGRI24NQRRGUPD46Q45MGMXZMRTQFXS25Y4NZVNPT34GM6S", AuctionTypeUserLiquidation, "GAUPKZVR6HID4CUJ5SF3VCV4VRZTXAL2XLJTCYTTDPMHAYBTPXQW6IQO", "GAVG3ODZ4SAVK2WJL3F3RT265RL7P6QNOMA6NL3XDAREKSX3OWWMXF4R", 3, 0, "fb5acf38"},
		{62_625_124, v1PoolCDE6, AuctionTypeUserLiquidation, "GA3COWJGKAWTO3TOWLBKN6CHWJCSRTPASFZVNN3X3MRR6W4SAUEOYEYX", "GBPUFOPDVPGM56MIO4KYSNV2N4XLYBZAXL6QMD75JWG7GC5L6ANFSYBL", 100, 0, "2b87c1e9"},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(FillAuctionEvent)
		if !ok {
			t.Fatalf("ledger %d: not a FillAuctionEvent", c.ledger)
		}
		if out.Pool != c.pool || out.AuctionType != c.typ || out.User != c.user || out.Filler != c.filler {
			t.Errorf("ledger %d: got pool=%s type=%d user=%s filler=%s, want %s %d %s %s",
				c.ledger, out.Pool, out.AuctionType, out.User, out.Filler, c.pool, c.typ, c.user, c.filler)
		}
		if out.FillPercent == nil || out.FillPercent.Cmp(big.NewInt(c.pct)) != 0 {
			t.Errorf("ledger %d: FillPercent=%v want %d", c.ledger, out.FillPercent, c.pct)
		}
		if out.Ledger != c.ledger || out.EventIndex != c.evIndex || !strings.HasPrefix(out.TxHash, c.txPrefix) {
			t.Errorf("ledger %d: coords ledger=%d ei=%d tx=%s", c.ledger, out.Ledger, out.EventIndex, out.TxHash)
		}
		if out.Data != nil {
			t.Errorf("ledger %d: Data=%+v, want nil", c.ledger, out.Data)
		}
	}
}

// V1 new_auction topics are [Symbol, u32(auction_type)] with no user: the
// user is the V1 backstop and percent is 100. The bad_debt rows one ledger
// earlier move exactly the bid's dTokens onto the backstop, which is the
// evidence that the auction covers the whole liability.
func TestGolden_NewAuctionV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger uint32
		pool   string
		typ    uint32
		bid    map[string]int64
		lot    map[string]int64
		block  uint32
	}{
		{
			52_428_305, v1PoolCDVQ, AuctionTypeBadDebt,
			map[string]int64{v1USDC: 8_565_694},
			map[string]int64{v1BLNDLP: 31_596_056},
			52_428_306,
		},
		{
			52_503_940, v1PoolCDVQ, AuctionTypeInterest,
			map[string]int64{v1BLNDLP: 270_358_315_554},
			map[string]int64{v1XLM: 351_343_973, v1USDC: 59_928_017_284},
			52_503_941,
		},
		{
			55_570_396, v1PoolCDE6, AuctionTypeBadDebt,
			map[string]int64{"CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC": 11_642_263},
			map[string]int64{v1BLNDLP: 39_552_757},
			55_570_397,
		},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(NewAuctionEvent)
		if !ok {
			t.Fatalf("ledger %d: not a NewAuctionEvent", c.ledger)
		}
		if out.Pool != c.pool || out.AuctionType != c.typ || out.User != MainnetBackstopV1 || out.Percent != 100 {
			t.Errorf("ledger %d: got pool=%s type=%d user=%s percent=%d", c.ledger, out.Pool, out.AuctionType, out.User, out.Percent)
		}
		if out.Data.Block != c.block {
			t.Errorf("ledger %d: Block=%d want %d", c.ledger, out.Data.Block, c.block)
		}
		wantAmounts(t, "bid", out.Data.Bid, c.bid)
		wantAmounts(t, "lot", out.Data.Lot, c.lot)
	}
}

// V1 bad_debt: [Symbol, Address(user)], body (asset: Address, d_tokens: i128).
func TestGolden_BadDebtV1(t *testing.T) {
	t.Parallel()
	evs := loadV1PoolAuctionEvents(t)
	cases := []struct {
		ledger uint32
		pool   string
		user   string
		asset  string
		amount int64
	}{
		{52_428_304, v1PoolCDVQ, "GDTEIX6BJJCXUGUMIZDCLM5XMJOJZKU7HIGXVM37XYNOXLDICEFRURGK", v1USDC, 8_565_694},
		{55_570_394, v1PoolCDE6, "GDRRWCHXH5XMB54UAAD6TTYNHZTQ5QY7IKXMXZHGP2XA6P3FG75GJTUV", "CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC", 4_071_398},
		{55_570_395, v1PoolCDE6, "GCNILL7CSIEG5THCOUXZK6FYFKVRPGMPLTINSZE6MOK7BK7Q4CN4OWTZ", "CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC", 7_570_865},
	}
	for _, c := range cases {
		out, ok := decodeOne(t, evs[c.ledger]).(EmissionEvent)
		if !ok {
			t.Fatalf("ledger %d: not an EmissionEvent", c.ledger)
		}
		if out.Kind != EventBadDebt || out.Pool != c.pool || out.User != c.user || out.Asset != c.asset {
			t.Errorf("ledger %d: got kind=%s pool=%s user=%s asset=%s", c.ledger, out.Kind, out.Pool, out.User, out.Asset)
		}
		if out.Amount == nil || out.Amount.Cmp(big.NewInt(c.amount)) != 0 {
			t.Errorf("ledger %d: Amount=%v want %d", c.ledger, out.Amount, c.amount)
		}
	}
}

func wantAmounts(t *testing.T, label string, got []AssetAmount, want map[string]int64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d entries, want %d (%+v)", label, len(got), len(want), got)
	}
	for _, a := range got {
		w, ok := want[a.Asset.String()]
		if !ok || a.Amount.Cmp(big.NewInt(w)) != 0 {
			t.Errorf("%s: %s=%v, want %v", label, a.Asset.String(), a.Amount, w)
		}
	}
}

const (
	v1PoolCDVQ = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
	v1PoolCDE6 = "CDE65QK2ROZ32V2LVLBOKYPX47TYMYO37Z6ASQTBRTBNK53C7C6QF4Y7"
	v1USDC     = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	v1XLM      = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	v1BLNDLP   = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM"
)
