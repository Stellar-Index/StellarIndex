package defindex

import (
	"bufio"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// adminFixture is one r1-lake stellar.contract_events row, as saved in
// test/fixtures/defindex/vault-admin-2026-09-30/admin_events.jsonl.
type adminFixture struct {
	Ledger     uint32   `json:"ledger_seq"`
	TxHash     string   `json:"tx_hash"`
	OpIndex    int      `json:"op_index"`
	EventIndex int      `json:"event_index"`
	ContractID string   `json:"contract_id"`
	Topics     []string `json:"topics_xdr"`
	Data       string   `json:"data_xdr"`
}

func loadAdminFixtures(t *testing.T) []adminFixture {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "..", "test", "fixtures", "defindex", "vault-admin-2026-09-30", "admin_events.jsonl"))
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	var out []adminFixture
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r adminFixture
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			t.Fatalf("fixture line %d: %v", len(out)+1, err)
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan fixture: %v", err)
	}
	return out
}

// TestGolden_defindexVaultAdmin drives every captured vault admin row
// through the production NewDecoder().Matches/Decode and pins the
// decoded values. Expected values were decoded from the same bytes
// independently of internal/scval (hand-rolled XDR + strkey); the
// strategy addresses are curated MainnetStrategies entries.
func TestGolden_defindexVaultAdmin(t *testing.T) {
	t.Parallel()

	const (
		gManager  = "GBXAQZ2DBYQCU26DWOHQR5UNGBG5JLT37LNNABSJGBQN2C7FXYUOQZGU"
		gCAARF    = "GBCPQMFNL6VMHHF4QGDAEXXTVUV3MXOVSQWLBETNRJTKPMB7QTOFFCG5"
		gRescuer1 = "GBBOK3GW3RDQTTQ7DSRA6JYQVMQLVBSL4PGBSYJC6L66Z72OHXEY3UPM"
		gRescuer2 = "GBDEH6C5UICDFKU5CCSM7INYG6TQBVZRTJ7NRWH5NYVHA4EPAPBSE4M2"
		gUnpause  = "GDTOT74GSDGVHPLE3SOT6PSGGR7CTJCZGHJJMOGIBR52DDRYQUL2NUKL"
		sCCSRX    = "CCSRX5E4337QMCMC3KO3RDFYI57T5NZV5XB3W3TWE4USCASKGL5URKJL"
		sCBTSR    = "CBTSRJLN5CVVOWLTH2FY5KNQ47KW5KKU3VWGASDN72STGMXLRRNHPRIL"
		sCDB2W    = "CDB2WMKQQNVZMEBY7Q7GZ5C7E7IAFSNMZ7GGVD6WKTCEWK7XOIAVZSAP"
	)
	type key struct {
		ledger uint32
		ev     int
	}
	want := map[key]VaultAdmin{
		{60058071, 15}: {Kind: EventNManager, NewAddress: gManager},
		{60058106, 10}: {Kind: EventNManager, NewAddress: gManager},
		{60058200, 10}: {Kind: EventNManager, NewAddress: gManager},
		{60966305, 0}:  {Kind: EventPaused, Caller: gCAARF, Strategy: sCCSRX},
		{60966313, 0}:  {Kind: EventUnpaused, Caller: gCAARF, Strategy: sCCSRX},
		{60966338, 0}:  {Kind: EventNEManager, NewAddress: gCAARF},
		{60966341, 0}:  {Kind: EventRBManager, NewAddress: gCAARF},
		{60966344, 0}:  {Kind: EventNReceiver, Caller: gCAARF, NewAddress: gCAARF},
		{61352469, 9}:  {Kind: EventRescue, Caller: gRescuer1, Strategy: sCCSRX},
		{61353122, 9}:  {Kind: EventRescue, Caller: gRescuer1, Strategy: sCBTSR},
		{61354348, 9}:  {Kind: EventRescue, Caller: gRescuer2, Strategy: sCCSRX},
		{62674637, 0}:  {Kind: EventUnpaused, Caller: gUnpause, Strategy: sCDB2W},
	}
	wantAmount := map[key]string{
		{61352469, 9}: "152005994675",
		{61353122, 9}: "77968348196",
		{61354348, 9}: "94771026693",
	}

	rows := loadAdminFixtures(t)
	if len(rows) != len(want) {
		t.Fatalf("fixture has %d rows, want %d", len(rows), len(want))
	}
	d := NewDecoder()
	for _, r := range rows {
		k := key{r.Ledger, r.EventIndex}
		w, ok := want[k]
		if !ok {
			t.Fatalf("fixture row %v has no expectation", k)
		}
		ev := events.Event{
			Type:       "contract",
			ContractID: r.ContractID,
			Ledger:     r.Ledger,
			// Not part of the capture; only the header copy is checked.
			LedgerClosedAt: "2026-01-01T00:00:00Z",
			TxHash:         r.TxHash,
			OperationIndex: r.OpIndex,
			EventIndex:     r.EventIndex,
			Topic:          r.Topics,
			Value:          r.Data,
		}
		if !d.Matches(ev) {
			t.Errorf("%v: Matches = false — a curated vault's admin event must pass the gate", k)
			continue
		}
		out, err := d.Decode(ev)
		if err != nil {
			t.Errorf("%v: Decode err = %v", k, err)
			continue
		}
		if len(out) != 1 {
			t.Errorf("%v: Decode emitted %d events, want 1", k, len(out))
			continue
		}
		ae, ok := out[0].(AdminEvent)
		if !ok {
			t.Errorf("%v: Decode emitted %T, want AdminEvent", k, out[0])
			continue
		}
		got := ae.Admin
		if got.Kind != w.Kind || got.Caller != w.Caller || got.Strategy != w.Strategy || got.NewAddress != w.NewAddress {
			t.Errorf("%v: got kind=%q caller=%q strategy=%q new=%q, want kind=%q caller=%q strategy=%q new=%q",
				k, got.Kind, got.Caller, got.Strategy, got.NewAddress, w.Kind, w.Caller, w.Strategy, w.NewAddress)
		}
		if got.Vault != r.ContractID || got.TxHash != r.TxHash || got.Ledger != r.Ledger ||
			got.OpIndex != r.OpIndex || int(got.EventIndex) != r.EventIndex {
			t.Errorf("%v: header not preserved: %+v", k, got)
		}
		wa, hasAmount := wantAmount[k]
		switch {
		case hasAmount && got.Amount == nil:
			t.Errorf("%v: Amount = nil, want %s", k, wa)
		case hasAmount && got.Amount.String() != wa:
			t.Errorf("%v: Amount = %s, want %s", k, got.Amount, wa)
		case !hasAmount && got.Amount != nil:
			t.Errorf("%v: Amount = %s, want nil (no amount in a %s body)", k, got.Amount, got.Kind)
		}
		if ae.EventKind() != "defindex.vault.admin" {
			t.Errorf("%v: EventKind = %q", k, ae.EventKind())
		}
	}
}

// TestDecode_vaultAdminMalformed: a modelled admin topic whose body lacks
// or mistypes a required field is a decode error, never a partial row.
func TestDecode_vaultAdminMalformed(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	g := addrSCVal(makeAccountAddress(t, 0xAA))
	c := addrSCVal(makeContractAddress(t, 0xBB))
	cases := map[string]struct {
		sym  string
		body string
	}{
		"nmanager missing new_manager": {TopicSymbolNManager, mustB64(t, mapSCVal(t, mapEntry(t, "manager", g)))},
		"nreceiver missing caller":     {TopicSymbolNReceiver, mustB64(t, mapSCVal(t, mapEntry(t, "new_fee_receiver", g)))},
		"paused strategy not address":  {TopicSymbolPaused, mustB64(t, mapSCVal(t, mapEntry(t, "caller", g), mapEntry(t, "strategy_address", symSCVal("x"))))},
		"rescue missing amount": {TopicSymbolRescue, mustB64(t, mapSCVal(t,
			mapEntry(t, "caller", g), mapEntry(t, "strategy_address", c)))},
		"rescue negative amount": {TopicSymbolRescue, mustB64(t, mapSCVal(t,
			mapEntry(t, "amount_withdrawn", i128SCVal(big.NewInt(-1))),
			mapEntry(t, "caller", g), mapEntry(t, "strategy_address", c)))},
		"body not a map": {TopicSymbolRBManager, mustB64(t, g)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{
				ContractID:     MainnetVaults[0],
				Ledger:         61_000_000,
				LedgerClosedAt: "2026-01-01T00:00:00Z",
				TxHash:         "admintx",
				Topic:          []string{TopicPrefixVault, tc.sym},
				Value:          tc.body,
			}
			out, err := d.Decode(ev)
			if !errors.Is(err, ErrMalformedPayload) {
				t.Errorf("err = %v, want ErrMalformedPayload", err)
			}
			if len(out) != 0 {
				t.Errorf("emitted %d events, want 0", len(out))
			}
		})
	}
}
