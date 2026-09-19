package controlwiring

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
	"github.com/Stellar-Index/StellarIndex/internal/sources/phoenix"
)

// Real lake captures of the phoenix factory's events (F048). These are
// rows of r1's stellar.contract_events, byte-for-byte; the queries that
// produced them sit beside them. See the directory's README.md.
const (
	phoenixFactoryFixtureDir = "test/fixtures/phoenix/factory-create"
	phoenixCreates2024       = "create_2024-05-07_ledgers_51572026-51572101.jsonl"
	phoenixFactory2026       = "factory_2026-07-02_ledgers_63293663-63293708.jsonl"
)

// lakeContractEventRow is one JSONEachRow line of stellar.contract_events
// as the capture queries select it.
type lakeContractEventRow struct {
	LedgerSeq        uint32   `json:"ledger_seq"`
	CloseTime        string   `json:"close_time"`
	TxHash           string   `json:"tx_hash"`
	OpIndex          int      `json:"op_index"`
	EventIndex       int      `json:"event_index"`
	ContractID       string   `json:"contract_id"`
	EventType        string   `json:"event_type"`
	Topic0Sym        string   `json:"topic_0_sym"`
	TopicsXDR        []string `json:"topics_xdr"`
	DataXDR          string   `json:"data_xdr"`
	InSuccessfulCall int      `json:"in_successful_call"`
}

// event rebuilds the dispatcher-facing event from a lake row, the same
// fields the ClickHouse streamer populates.
func (r lakeContractEventRow) event() events.Event {
	return events.Event{
		Type:                     r.EventType,
		Ledger:                   r.LedgerSeq,
		LedgerClosedAt:           strings.Replace(r.CloseTime, " ", "T", 1) + "Z",
		ContractID:               r.ContractID,
		TxHash:                   r.TxHash,
		OperationIndex:           r.OpIndex,
		EventIndex:               r.EventIndex,
		InSuccessfulContractCall: r.InSuccessfulCall == 1,
		Topic:                    r.TopicsXDR,
		Value:                    r.DataXDR,
	}
}

func loadPhoenixFactoryRows(t *testing.T, name string) []lakeContractEventRow {
	t.Helper()
	path := filepath.Join(repoRoot(t), phoenixFactoryFixtureDir, name)
	f, err := os.Open(path) //nolint:gosec // repo-relative, test-only
	if err != nil {
		t.Fatalf("open fixture %s: %v", name, err)
	}
	defer func() { _ = f.Close() }()

	var rows []lakeContractEventRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row lakeContractEventRow
		if uerr := json.Unmarshal([]byte(line), &row); uerr != nil {
			t.Fatalf("fixture %s: %v", name, uerr)
		}
		rows = append(rows, row)
	}
	if serr := sc.Err(); serr != nil {
		t.Fatalf("scan fixture %s: %v", name, serr)
	}
	if len(rows) == 0 {
		t.Fatalf("fixture %s holds no rows", name)
	}
	return rows
}

// isPhoenixFactoryCreate reports whether a lake row is the factory's
// ("create","liquidity_pool") event, by decoding the topics rather than
// trusting topic_0_sym (which the lake leaves empty for String topics).
func isPhoenixFactoryCreate(t *testing.T, r lakeContractEventRow) bool {
	t.Helper()
	if len(r.TopicsXDR) != 2 {
		return false
	}
	t0, err := scval.Parse(r.TopicsXDR[0])
	if err != nil {
		t.Fatalf("ledger %d topic[0]: %v", r.LedgerSeq, err)
	}
	s, err := scval.AsText(t0)
	return err == nil && s == "create"
}

// phoenixAnnouncedPool decodes a create row's body: one contract Address.
func phoenixAnnouncedPool(t *testing.T, r lakeContractEventRow) string {
	t.Helper()
	body, err := scval.Parse(r.DataXDR)
	if err != nil {
		t.Fatalf("ledger %d body: %v", r.LedgerSeq, err)
	}
	if body.Type != xdr.ScValTypeScvAddress {
		t.Fatalf("ledger %d body is %s, want a single ScvAddress", r.LedgerSeq, body.Type)
	}
	announcedPool, err := scval.AsAddressStrkey(body)
	if err != nil {
		t.Fatalf("ledger %d body address: %v", r.LedgerSeq, err)
	}
	return announcedPool
}

// phoenixCreateRows returns every factory create event across both
// captures, in ledger order.
func phoenixCreateRows(t *testing.T) []lakeContractEventRow {
	t.Helper()
	var out []lakeContractEventRow
	for _, name := range []string{phoenixCreates2024, phoenixFactory2026} {
		for _, r := range loadPhoenixFactoryRows(t, name) {
			if isPhoenixFactoryCreate(t, r) {
				out = append(out, r)
			}
		}
	}
	return out
}

// TestPhoenixFactoryCreateFixture_Shape pins what the real captures
// settle about the factory's creation event, so nobody has to take a
// comment's word for it again:
//
//   - the events ARE in the lake, from ledger 51,572,026 — ten ledgers
//     after the factory's own genesis. "The factory's creation events
//     predate the lake" was false.
//   - topic[0] and topic[1] are ScvString, not ScvSymbol, so the lake's
//     topic_0_sym column is EMPTY for them and a ClickHouse filter on
//     topic_0_sym = 'create' matches none of these rows.
//   - the body is ONE contract Address, the pool. No stake contract is
//     announced, so this event can never admit a stake.
//   - the shape is identical across the 2024 rows and the 2026 row
//     emitted after the Map-schema pool WASM.
//   - each announced address is a pool the curated seed already lists,
//     at the ledger its seed comment cites — independent corroboration
//     that the curated set and the factory agree on history.
func TestPhoenixFactoryCreateFixture_Shape(t *testing.T) {
	t.Parallel()
	// announced pool by creation ledger, from the curated seed's own
	// comments in internal/sources/phoenix/events.go.
	wantPool := map[uint32]string{
		51_572_026: "CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX",
		51_572_030: "CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH",
		51_572_101: "CAZ6W4WHVGQBGURYTUOLCUOOHW6VQGAAPSPCD72VEDZMBBPY7H43AYEC",
		63_293_708: "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S",
	}
	curated := make(map[string]bool)
	for _, p := range phoenix.MainnetPools {
		curated[p] = true
	}
	for _, p := range phoenix.MainnetMapPools {
		curated[p] = true
	}

	rows := phoenixCreateRows(t)
	if len(rows) != len(wantPool) {
		t.Fatalf("captures hold %d create events, want %d", len(rows), len(wantPool))
	}
	for _, r := range rows {
		if r.ContractID != phoenix.MainnetFactory {
			t.Errorf("ledger %d: emitter %s is not the factory", r.LedgerSeq, r.ContractID)
		}
		if r.Topic0Sym != "" {
			t.Errorf("ledger %d: lake topic_0_sym = %q, want empty (String topic)", r.LedgerSeq, r.Topic0Sym)
		}
		for i, want := range []string{"create", "liquidity_pool"} {
			sv, err := scval.Parse(r.TopicsXDR[i])
			if err != nil {
				t.Fatalf("ledger %d topic[%d]: %v", r.LedgerSeq, i, err)
			}
			if sv.Type != xdr.ScValTypeScvString {
				t.Errorf("ledger %d topic[%d] is %s, want ScvString", r.LedgerSeq, i, sv.Type)
			}
			if got, _ := scval.AsString(sv); got != want {
				t.Errorf("ledger %d topic[%d] = %q, want %q", r.LedgerSeq, i, got, want)
			}
			if r.TopicsXDR[i] != scval.MustEncodeString(want) {
				t.Errorf("ledger %d topic[%d] is not byte-equal to MustEncodeString(%q)", r.LedgerSeq, i, want)
			}
		}
		announcedPool := phoenixAnnouncedPool(t, r)
		if !strings.HasPrefix(announcedPool, "C") {
			t.Errorf("ledger %d announces %s, want a contract address", r.LedgerSeq, announcedPool)
		}
		if announcedPool != wantPool[r.LedgerSeq] {
			t.Errorf("ledger %d announces %s, want %s", r.LedgerSeq, announcedPool, wantPool[r.LedgerSeq])
		}
		if !curated[announcedPool] {
			t.Errorf("ledger %d announces %s, which the curated seed does not list", r.LedgerSeq, announcedPool)
		}
	}
}

// TestPhoenixFactoryCreateFixture_UpdatedConfigIsNotACreate guards the
// one non-creation row in the 2026 capture: ("Factory","Updated Config")
// with body `true`. Anything that later classifies factory events must
// not read it as a pool announcement.
func TestPhoenixFactoryCreateFixture_UpdatedConfigIsNotACreate(t *testing.T) {
	t.Parallel()
	var other []lakeContractEventRow
	for _, r := range loadPhoenixFactoryRows(t, phoenixFactory2026) {
		if !isPhoenixFactoryCreate(t, r) {
			other = append(other, r)
		}
	}
	if len(other) != 1 {
		t.Fatalf("2026 capture holds %d non-create rows, want 1", len(other))
	}
	body, err := scval.Parse(other[0].DataXDR)
	if err != nil {
		t.Fatalf("updated-config body: %v", err)
	}
	if body.Type == xdr.ScValTypeScvAddress {
		t.Errorf("the Updated Config body is an Address; it must not be mistakable for a pool announcement")
	}
	if dec := phoenix.NewDecoder(); dec.Matches(other[0].event()) {
		t.Errorf("phoenix.Decoder matches the factory's (\"Factory\",\"Updated Config\") event")
	}
}
