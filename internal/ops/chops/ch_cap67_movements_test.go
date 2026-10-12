package chops

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	sep41supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	sep41 "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_transfers"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func encScVal(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func scAddr(t *testing.T, g string) xdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteAccountID, g)
	if err != nil {
		t.Fatalf("decode %q: %v", g, err)
	}
	var pub xdr.Uint256
	copy(pub[:], raw)
	aid := xdr.AccountId{Type: xdr.PublicKeyTypePublicKeyTypeEd25519, Ed25519: &pub}
	a := xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeAccount, AccountId: &aid}
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &a}
}

func scI128(n int64) xdr.ScVal {
	p := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)} //nolint:gosec // test literal
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &p}
}

// cap67TransferEvent builds a decodable CAP-67 transfer event — the
// 4-topic classic-asset shape when sep0011 != "", else the 3-topic pure
// Soroban-token shape.
func cap67TransferEvent(t *testing.T, sep0011 string) events.Event {
	t.Helper()
	topics := []string{
		scval.MustEncodeSymbol("transfer"),
		encScVal(t, scAddr(t, "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4")),
		encScVal(t, scAddr(t, "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")),
	}
	if sep0011 != "" {
		topics = append(topics, scval.MustEncodeString(sep0011))
	}
	return events.Event{
		Type:           "contract",
		Ledger:         63_000_000,
		LedgerClosedAt: "2026-08-01T00:00:00Z",
		ContractID:     "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA",
		TxHash:         "aa11223344556677889900112233445566778899001122334455667788990011",
		Topic:          topics,
		Value:          encScVal(t, scI128(12_345_678)),
	}
}

// TestCap67MovementFromEvent_NativeClassic: a native-XLM CAP-67 transfer from the deliberately-unwatched native SAC
// decodes into a movement with asset "native" and cap67 provenance.
func TestCap67MovementFromEvent_NativeClassic(t *testing.T) {
	dec := sep41.NewUngatedDecoder()
	ev := cap67TransferEvent(t, "native")

	m, ok := cap67MovementFromEvent(dec, &ev)
	if !ok {
		t.Fatal("cap67MovementFromEvent returned ok=false for a valid native transfer")
	}
	if m.Asset != "native" {
		t.Errorf("Asset = %q, want native", m.Asset)
	}
	if m.Provenance != "cap67_derived" {
		t.Errorf("Provenance = %q, want cap67_derived", m.Provenance)
	}
	if m.MovementKind != "transfer" {
		t.Errorf("MovementKind = %q, want transfer", m.MovementKind)
	}
	if m.FromAddress == "" || m.ToAddress == "" || m.Amount == nil {
		t.Fatalf("incomplete movement: from=%q to=%q amount=%v", m.FromAddress, m.ToAddress, m.Amount)
	}
	if m.Amount.Int64() != 12_345_678 {
		t.Errorf("Amount = %s, want 12345678", m.Amount)
	}
	// The ungated decoder must never be dispatcher-wireable by accident.
	if dec.Matches(ev) {
		t.Error("NewUngatedDecoder().Matches returned true — it must stay un-wireable in gated paths")
	}
}

const (
	cap67NativeSAC = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	cap67USDCSAC   = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	cap67USDCName  = "USDC:GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	// A WASM contract that is nobody's SAC (the Blend pool fixture id).
	cap67NonSAC = "CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP"
)

func TestCap67AssetName(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		emitter string
		sep0011 string
		want    string
	}{
		{cap67NativeSAC, "native", "native"},
		{cap67USDCSAC, cap67USDCName, "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"},
		// Malformed sep0011 falls back to the emitting contract id —
		// honest identity, never a fabricated classic id.
		{cap67NativeSAC, "not a real asset name", cap67NativeSAC},
		// 3-topic (no sep0011): pure Soroban token → contract id.
		{cap67NativeSAC, "", cap67NativeSAC},
	} {
		ev := cap67TransferEvent(t, tc.sep0011)
		ev.ContractID = tc.emitter
		if got := cap67AssetName(&ev); got != tc.want {
			t.Errorf("emitter=%s sep0011=%q: asset = %q, want %q", tc.emitter, tc.sep0011, got, tc.want)
		}
	}
}

// TestCap67AssetName_SpoofedTopicRendersAsEmitter: any contract can emit a 4-topic transfer whose last topic names a
// trusted asset. The label is trusted only when the emitter IS that asset's SAC; otherwise the row is the emitter's own
// token.
func TestCap67AssetName_SpoofedTopicRendersAsEmitter(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		name, emitter, sep0011 string
	}{
		{"non-SAC claims USDC", cap67NonSAC, cap67USDCName},
		{"non-SAC claims native", cap67NonSAC, "native"},
		{"native SAC claims USDC", cap67NativeSAC, cap67USDCName},
		{"USDC SAC claims native", cap67USDCSAC, "native"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := cap67TransferEvent(t, tc.sep0011)
			ev.ContractID = tc.emitter
			m, ok := cap67MovementFromEvent(sep41.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67MovementFromEvent returned ok=false")
			}
			if m.Asset != tc.emitter {
				t.Errorf("asset = %q, want the emitting contract %q", m.Asset, tc.emitter)
			}
		})
	}
}

// TestCap67AssetName_NetworkAware — the SAC cross-check derives against the
// -network the job runs with: on testnet the pubnet native SAC is not XLM's
// SAC, and the testnet one is.
func TestCap67AssetName_NetworkAware(t *testing.T) {
	pass, err := cap67NetworkPassphrase("testnet")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { canonical.InstallNetworkPassphrase("") })
	canonical.InstallNetworkPassphrase(pass)
	testnetNativeSAC, err := canonical.NativeAsset().SacContractID()
	if err != nil {
		t.Fatal(err)
	}
	if testnetNativeSAC == cap67NativeSAC {
		t.Fatal("testnet native SAC equals pubnet's — passphrase not applied")
	}
	for emitter, want := range map[string]string{
		testnetNativeSAC: "native",
		cap67NativeSAC:   cap67NativeSAC,
	} {
		ev := cap67TransferEvent(t, "native")
		ev.ContractID = emitter
		if got := cap67AssetName(&ev); got != want {
			t.Errorf("emitter=%s: asset = %q, want %q", emitter, got, want)
		}
	}
}

func TestCap67NetworkPassphrase(t *testing.T) {
	for name, want := range map[string]string{
		"pubnet":    canonical.PubnetPassphrase,
		"testnet":   canonical.TestnetPassphrase,
		"futurenet": canonical.FuturenetPassphrase,
	} {
		if got, err := cap67NetworkPassphrase(name); err != nil || got != want {
			t.Errorf("%s: got (%q, %v), want %q", name, got, err, want)
		}
	}
	if _, err := cap67NetworkPassphrase("mainnet"); err == nil {
		t.Error("unknown network accepted; want an error")
	}
}

// The idle-tick diagnostic read (LakeMinLedger) is purely diagnostic: cap67IdleLogTick must swallow its failure into the
// log line. Propagating it as a catchUp error would count as neither work nor idleness, freezing the idle counter and
// retrying the read every tick.
func TestCap67IdleLogTick_MinLedgerFailureDoesNotPropagate(t *testing.T) {
	res := cap67CatchUp{start: 100, last: 99} // idle: last < start
	failingMinLedger := func(context.Context, string) (uint32, error) {
		return 0, errors.New("clickhouse: lake min ledger: connection refused")
	}

	out := captureStderr(t, func() {
		cap67IdleLogTick(context.Background(), "example.com:9000", res, 30, time.Second, failingMinLedger)
	})

	if !strings.Contains(out, "idle for 30 ticks") {
		t.Fatalf("diagnostic line missing even though the failure must still be logged; got: %q", out)
	}
	if !strings.Contains(out, "unavailable") || !strings.Contains(out, "connection refused") {
		t.Errorf("diagnostic line must name the min-present read failure; got: %q", out)
	}
}

// TestResolveStart pins the first-run clamp: a genesis floor of 1 against a lake starting at ledger 2 must start at 2.
// ContiguousWatermark treats a `from` the lake lacks as a boundary hole and answers from-1 forever, idling the daemon;
// resumed runs and higher floors are unchanged.
func TestResolveStart(t *testing.T) {
	for _, tc := range []struct {
		name               string
		wm, floor, lakeMin uint32
		want               uint32
	}{
		{"test net first run: floor genesis, lake begins at 2", 0, 1, 2, 2},
		{"resumed run continues at wm+1 whatever the floor and lake", 64_476_027, 1, 2, 64_476_028},
		{"resumed run ignores a lake min above the watermark", 100, 1, 500, 101},
		{"pubnet first run: floor above the lake's start stays the floor", 0, 58_762_517, 2, 58_762_517},
		{"floor equal to the lake's first ledger", 0, 2, 2, 2},
		{"empty lake leaves the floor alone (idles until the lake reaches it)", 0, 1, 0, 1},
		{"partial lake beginning above genesis clamps up to it", 0, 1, 4_500_000, 4_500_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveStart(tc.wm, tc.floor, tc.lakeMin); got != tc.want {
				t.Fatalf("resolveStart(wm=%d, floor=%d, lakeMin=%d) = %d, want %d",
					tc.wm, tc.floor, tc.lakeMin, got, tc.want)
			}
		})
	}
}

// TestFollowTick pins the idle backoff: base tick through cap67IdleBackoffAfter idle ticks (a live feed idles a few
// ticks per ledger), then doubling up to cap67IdleMaxInterval, never below the operator's base.
func TestFollowTick(t *testing.T) {
	const base = time.Second
	for _, tc := range []struct {
		name string
		base time.Duration
		idle int
		want time.Duration
	}{
		{"just worked", base, 0, base},
		{"a few idle ticks inside one ledger cadence", base, 4, base},
		{"at the threshold still the base", base, cap67IdleBackoffAfter, base},
		{"one past the threshold doubles", base, cap67IdleBackoffAfter + 1, 2 * base},
		{"two past the threshold doubles again", base, cap67IdleBackoffAfter + 2, 4 * base},
		{"five past the threshold is capped", base, cap67IdleBackoffAfter + 5, cap67IdleMaxInterval},
		{"a long stall stays capped", base, 10_000, cap67IdleMaxInterval},
		{"a base above the cap is never shortened", 2 * cap67IdleMaxInterval, 10_000, 2 * cap67IdleMaxInterval},
		{"a sub-second base backs off from its own value", 500 * time.Millisecond, cap67IdleBackoffAfter + 3, 4 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := followTick(tc.base, tc.idle); got != tc.want {
				t.Fatalf("followTick(%s, idle=%d) = %s, want %s", tc.base, tc.idle, got, tc.want)
			}
		})
	}
}

// A dry run sizes a window in the rows a write run inserts: a two-sided
// transfer fans out to a sent row and a received row, so one event is two
// rows, not one.
func TestDeriveCap67MovementsWindow_DryRunCountsFannedOutRows(t *testing.T) {
	orig := streamCap67TransferEvents
	t.Cleanup(func() { streamCap67TransferEvents = orig })
	streamCap67TransferEvents = func(_ context.Context, _ string, _, _ uint32, _, _, _ []string, _, _, _ bool, fn func(events.Event) error) error {
		for _, sep0011 := range []string{"native", ""} {
			if err := fn(cap67TransferEvent(t, sep0011)); err != nil {
				return err
			}
		}
		return nil
	}

	rows, skipped, err := deriveCap67MovementsWindow(context.Background(), "ch", 63_000_000, 63_000_000, true)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if rows != 4 {
		t.Fatalf("dry-run rows = %d, want 4 (two transfers × sent+received)", rows)
	}
}

// The window derive must ask the lake for mint, burn and clawback as well as
// transfer, and write every one of them. The stub honours the topic[0]
// prefilter the way the SQL does.
func TestDeriveCap67MovementsWindow_IncludesSupplyKinds(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	orig := streamCap67TransferEvents
	t.Cleanup(func() { streamCap67TransferEvents = orig })
	lake := []struct {
		sym string
		ev  events.Event
	}{
		{"transfer", cap67TransferEvent(t, "native")},
		{"mint", cap67SupplyEvent(t, "mint", cap67USDCSAC, cap67USDCName, scI128(1))},
		{"burn", cap67SupplyEvent(t, "burn", cap67USDCSAC, cap67USDCName, scI128(1))},
		{"clawback", cap67SupplyEvent(t, "clawback", cap67USDCSAC, cap67USDCName, scI128(1))},
	}
	streamCap67TransferEvents = func(_ context.Context, _ string, _, _ uint32, _, topic0Syms, _ []string, _, _, _ bool, fn func(events.Event) error) error {
		for _, l := range lake {
			if !slices.Contains(topic0Syms, l.sym) {
				continue
			}
			if err := fn(l.ev); err != nil {
				return err
			}
		}
		return nil
	}

	rows, skipped, err := deriveCap67MovementsWindow(context.Background(), "ch", 63_000_000, 63_000_000, true)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if skipped != 0 {
		t.Fatalf("skipped = %d, want 0", skipped)
	}
	if rows != 8 {
		t.Fatalf("dry-run rows = %d, want 8 (transfer, mint, burn, clawback × two sides)", rows)
	}
}

// gaugeValue pulls one metric's sample from a textfile body, failing the test if the series is absent.
// Local copy: opsutil.gaugeValue is unexported across the package boundary.
func gaugeValue(t *testing.T, body, metric string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, metric+"{") {
			fields := strings.Fields(line)
			if len(fields) != 2 {
				t.Fatalf("malformed sample line %q", line)
			}
			return fields[1]
		}
	}
	t.Fatalf("metric %q is ABSENT from the textfile:\n%s", metric, body)
	return ""
}

// TestCap67Progress_AccumulatesAcrossWindowsAndTicks pins the cap67-movements progress metric: a wedged -follow daemon
// holds the watermark fixed, so it needs a heartbeat that differs from a healthy one. record runs per derive window and
// per tick, so total must be the running SUM, not the last count.
func TestCap67Progress_AccumulatesAcrossWindowsAndTicks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ops_job_ch_cap67_movements.prom")

	prog := newCap67Progress(path)
	if !prog.hb.Enabled() {
		t.Fatal("explicit path must enable the heartbeat")
	}

	// Two windows within one catch-up, then a later -follow tick — the
	// shape runCap67CatchUp / runCap67Follow drive this callback with.
	prog.record(5, 63_100_000)
	prog.record(3, 63_150_000)
	prog.record(2, 63_150_100)
	prog.stop(true)

	raw, err := os.ReadFile(path) //nolint:gosec // test-controlled temp path
	if err != nil {
		t.Fatalf("heartbeat textfile not written: %v", err)
	}
	body := string(raw)

	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_total"); got != "10" {
		t.Errorf("progress_total = %s, want 10 (5+3+2, cumulative across calls)", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_progress_cursor"); got != "63150100" {
		t.Errorf("progress_cursor = %s, want the highest ledger reached (63150100)", got)
	}
	if !strings.Contains(body, `{ops_job="ch-cap67-movements"}`) {
		t.Errorf("series must carry ops_job=\"ch-cap67-movements\":\n%s", body)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_running"); got != "0" {
		t.Errorf("running after stop = %s, want 0", got)
	}
	if got := gaugeValue(t, body, "stellarindex_ops_job_last_exit_ok"); got != "1" {
		t.Errorf("last_exit_ok = %s, want 1", got)
	}
}

// TestCap67Progress_InertWithoutHeartbeatPath: an empty -heartbeat flag (the default, and every non-r1 environment)
// makes the heartbeat a safe no-op, with no directory created and no panic on record/stop with an unresolved path.
func TestCap67Progress_InertWithoutHeartbeatPath(t *testing.T) {
	prog := newCap67Progress("")
	if prog.hb.Enabled() {
		t.Fatal("empty path without DefaultTextfileDir present must stay inert")
	}
	prog.record(1, 100)
	prog.stop(false)
}

const (
	cap67Holder     = "GDUY7J7A33TQWOSOQGDO776GGLM3UQERL4J3SPT56F6YS4ID7MLDERI4"
	cap67USDCIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
)

// cap67SupplyEvent builds a CAP-67 [kind, holder, sep0011] supply event, or
// the bare [kind, holder] SEP-41 shape when sep0011 is "".
func cap67SupplyEvent(t *testing.T, kind, emitter, sep0011 string, body xdr.ScVal) events.Event {
	t.Helper()
	topics := []string{scval.MustEncodeSymbol(kind), encScVal(t, scAddr(t, cap67Holder))}
	if sep0011 != "" {
		topics = append(topics, scval.MustEncodeString(sep0011))
	}
	return events.Event{
		Type:           "contract",
		Ledger:         63_000_000,
		LedgerClosedAt: "2026-08-01T00:00:00Z",
		ContractID:     emitter,
		TxHash:         "bb11223344556677889900112233445566778899001122334455667788990011",
		OperationIndex: 1,
		EventIndex:     2,
		Topic:          topics,
		Value:          encScVal(t, body),
	}
}

// From P23 on a classic payment from an issuer is a CAP-67 `mint` and one to
// it a `burn`; the derive must carry both sides so the issuer's own feed
// shows them, framed as the pre-P23 archive frames a clawback.
func TestCap67SupplyMovement_SACIssuerSide(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		kind     string
		from, to string
	}{
		{sep41supply.SymbolMint, cap67USDCIssuer, cap67Holder},
		{sep41supply.SymbolBurn, cap67Holder, cap67USDCIssuer},
		{sep41supply.SymbolClawback, cap67Holder, cap67USDCIssuer},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			ev := cap67SupplyEvent(t, tc.kind, cap67USDCSAC, cap67USDCName, scI128(5_000_000))
			m, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67EventMovement returned ok=false for a valid SAC supply event")
			}
			if m.MovementKind != tc.kind || m.Provenance != clickhouse.ProvenanceCAP67Derived {
				t.Errorf("kind/provenance = %q/%q, want %q/cap67_derived", m.MovementKind, m.Provenance, tc.kind)
			}
			if m.Asset != "USDC-"+cap67USDCIssuer {
				t.Errorf("asset = %q, want the canonical USDC id", m.Asset)
			}
			if m.FromAddress != tc.from || m.ToAddress != tc.to {
				t.Errorf("from/to = %q/%q, want %q/%q", m.FromAddress, m.ToAddress, tc.from, tc.to)
			}
			if m.Amount == nil || m.Amount.Int64() != 5_000_000 {
				t.Errorf("amount = %v, want 5000000", m.Amount)
			}
			if m.OpIndex != 1 || m.LegIndex != 2 {
				t.Errorf("op/leg = %d/%d, want 1/2", m.OpIndex, m.LegIndex)
			}
			rows := clickhouse.FanOutAccountMovement(m)
			if len(rows) != 2 || !slices.ContainsFunc(rows, func(r clickhouse.AccountMovementRow) bool { return r.Address == cap67USDCIssuer }) {
				t.Errorf("fan-out %+v has no issuer row", rows)
			}
		})
	}
}

// The issuer side is asserted only for a verified SAC: a contract that names
// USDC in its topic is not USDC, and a custom token's admin is not the
// counterparty of record, so only the holder's row is derived.
func TestCap67SupplyMovement_NonSACIsHolderOnly(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	for _, tc := range []struct {
		name, kind, sep0011 string
		wantDir             clickhouse.AccountMovementDirection
	}{
		{"spoofed USDC mint", sep41supply.SymbolMint, cap67USDCName, clickhouse.AccountMovementReceived},
		{"bare SEP-41 mint", sep41supply.SymbolMint, "", clickhouse.AccountMovementReceived},
		{"spoofed USDC burn", sep41supply.SymbolBurn, cap67USDCName, clickhouse.AccountMovementSent},
		{"clawback", sep41supply.SymbolClawback, "TOKEN", clickhouse.AccountMovementSent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ev := cap67SupplyEvent(t, tc.kind, cap67NonSAC, tc.sep0011, scI128(7))
			m, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
			if !ok {
				t.Fatal("cap67EventMovement returned ok=false")
			}
			if m.Asset != cap67NonSAC {
				t.Errorf("asset = %q, want the emitting contract", m.Asset)
			}
			rows := clickhouse.FanOutAccountMovement(m)
			if len(rows) != 1 || rows[0].Address != cap67Holder || rows[0].Direction != tc.wantDir || rows[0].Counterparty != "" {
				t.Errorf("fan-out = %+v, want one %s row for the holder with no counterparty", rows, tc.wantDir)
			}
		})
	}
}

// A muxed or memo-stamped mint carries its amount in a CAP-67 map body.
func TestCap67SupplyMovement_MapBody(t *testing.T) {
	canonical.InstallNetworkPassphrase("")
	amount := scI128(42)
	key := xdr.ScSymbol("amount")
	m := xdr.ScMap{{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &key}, Val: amount}}
	mp := &m
	body := xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
	ev := cap67SupplyEvent(t, sep41supply.SymbolMint, cap67USDCSAC, cap67USDCName, body)
	got, ok := cap67EventMovement(sep41.NewUngatedDecoder(), sep41supply.NewUngatedDecoder(), &ev)
	if !ok || got.Amount == nil || got.Amount.Int64() != 42 {
		t.Fatalf("map-bodied mint: ok=%v amount=%v, want 42", ok, got.Amount)
	}
}

// TestFollowLoop_ErrorRetriesThenCancelExits pins two contracts: a transient catch-up error
// must NOT kill the loop (it retries next tick; the watermark holds, so no ledger is skipped),
// and a ctx-cancel between ticks ends the loop cleanly (return nil).
func TestFollowLoop_ErrorRetriesThenCancelExits(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var calls int32
	err := followLoop(ctx, time.Millisecond, func(context.Context, int) (bool, error) {
		switch atomic.AddInt32(&calls, 1) {
		case 1:
			return false, errors.New("transient CH blip") // must be retried, not fatal
		case 4:
			cancel() // after surviving the error + a few good ticks, shut down
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil (graceful shutdown)", err)
	}
	if n := atomic.LoadInt32(&calls); n < 4 {
		t.Fatalf("catchUp called %d times, want >=4 — a transient error killed the loop", n)
	}
}

// TestFollowLoop_MidDeriveCancelIsClean: SIGTERM during a catch-up must be a clean shutdown (return nil), not a logged
// derive failure. It captures stderr so deleting the ctx.Err() shortcut in followLoop turns the test red.
func TestFollowLoop_MidDeriveCancelIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = orig })

	loopErr := followLoop(ctx, time.Hour, func(c context.Context, _ int) (bool, error) {
		cancel()              // shutdown requested mid-derive
		return false, c.Err() // catchUp returns the ctx error, as runCap67CatchUp would
	})

	_ = w.Close()
	os.Stderr = orig
	out, _ := io.ReadAll(r)

	if loopErr != nil {
		t.Fatalf("mid-derive cancel returned %v, want nil", loopErr)
	}
	if strings.Contains(string(out), "catch-up error") {
		t.Fatalf("mid-derive shutdown logged a spurious error line: %q", out)
	}
}

// TestFollowLoop_RunsCatchUpImmediately: the first catch-up fires before the first tick, so a user opening the page does
// not wait an interval. A long interval, one immediate call, then cancel proves the loop does not block on the ticker
// first.
func TestFollowLoop_RunsCatchUpImmediately(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var called int32
	err := followLoop(ctx, time.Hour, func(context.Context, int) (bool, error) {
		atomic.AddInt32(&called, 1)
		cancel()
		return true, nil
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil", err)
	}
	if atomic.LoadInt32(&called) != 1 {
		t.Fatalf("catchUp called %d times, want exactly 1 (immediate, pre-tick)", called)
	}
}

// Pins the idle count handed to each tick: consecutive idle ticks, reset by a tick that worked, kept by one that errored.
// A miscount would hide a stall or back off a live feed. No timing asserted; policy is TestFollowTick.
func TestFollowLoop_IdleAccounting(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// What each tick reports back, in order; the loop's idle count on
	// entry to each tick is recorded and compared afterwards.
	script := []struct {
		worked bool
		err    error
	}{
		{worked: false},                // idle 0 → 1
		{worked: false},                // idle 1 → 2
		{err: errors.New("transient")}, // idle 2 → 2 (an error is neither)
		{worked: false},                // idle 2 → 3
		{worked: true},                 // idle 3 → 0
		{worked: false},                // idle 0 → 1
		{worked: true},                 // idle 1 → 0
		{worked: false},                // last tick: cancels
	}
	wantIdle := []int{0, 1, 2, 2, 3, 0, 1, 0}

	var got []int
	err := followLoop(ctx, time.Millisecond, func(_ context.Context, idle int) (bool, error) {
		got = append(got, idle)
		step := script[len(got)-1]
		if len(got) == len(script) {
			cancel()
		}
		return step.worked, step.err
	})
	if err != nil {
		t.Fatalf("followLoop returned %v, want nil", err)
	}
	if len(got) != len(wantIdle) {
		t.Fatalf("catchUp called %d times, want %d", len(got), len(wantIdle))
	}
	for i := range wantIdle {
		if got[i] != wantIdle[i] {
			t.Errorf("tick %d saw idle=%d, want %d (sequence %v)", i+1, got[i], wantIdle[i], got)
		}
	}
}
