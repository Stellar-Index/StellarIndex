package spectra

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const fixtureRoot = "../../../test/fixtures/spectra"

// Mainnet fixtures captured by scripts/dev/capture-spectra-fixtures.sh.
const (
	wrapperHash = "0e8d68e206d66b087a2e0b64430356aec834ff906260e057180702d74d6f81f4"
	ytHash      = "daeb931b9507440b6dc4f73256ac33c6089964f5d035f7eec5fcedc10e732c8e"
	wrapperID   = "CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO"
	ytID        = "CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G"
)

type fixture struct {
	ContractID     string   `json:"contract_id"`
	Ledger         uint32   `json:"ledger"`
	TxHash         string   `json:"tx_hash"`
	LedgerClosedAt string   `json:"ledger_closed_at"`
	Topics         []string `json:"topics"`
	Value          string   `json:"value"`
}

func loadFixture(t *testing.T, hash, name string) events.Event {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(fixtureRoot, hash, name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return events.Event{
		Type: "contract", ContractID: f.ContractID, Ledger: f.Ledger,
		LedgerClosedAt: f.LedgerClosedAt, TxHash: f.TxHash,
		Topic: f.Topics, Value: f.Value,
	}
}

func mustDecodeOne(t *testing.T, ev events.Event) Event {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatalf("Matches = false for gated %s from %s", classify(&ev), ev.ContractID)
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	got, ok := out[0].(Event)
	if !ok {
		t.Fatalf("Decode emitted %T, want spectra.Event", out[0])
	}
	return got
}

// wrap, sw-deJTRSY wrapper, ledger 64716536: 1e18 shares for 1e18 vault shares.
func TestGolden_wrap_ledger64716536(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	got := mustDecodeOne(t, ev)

	if got.Kind != EventWrap || got.ContractID != wrapperID {
		t.Errorf("kind/contract = %q/%q", got.Kind, got.ContractID)
	}
	if got.Shares.String() != "1000000000000000000" || got.VaultShares.String() != "1000000000000000000" {
		t.Errorf("shares/vault_shares = %s/%s, want 1e18/1e18", got.Shares, got.VaultShares)
	}
	if got.Caller == "" || got.Caller != got.Receiver || got.Owner != "" {
		t.Errorf("caller/receiver/owner = %q/%q/%q", got.Caller, got.Receiver, got.Owner)
	}
	if got.Ledger != 64716536 || got.TxHash != ev.TxHash || got.ObservedAt.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-01T15:27:42Z" {
		t.Errorf("identity = %d %s %s", got.Ledger, got.TxHash, got.ObservedAt)
	}
	if got.EventKind() != EventKind || got.Source() != SourceName {
		t.Errorf("identity = %q %q", got.EventKind(), got.Source())
	}
}

// unwrap shares wrap's body; its shape is from source, so the proven wrap
// body is replayed under unwrap's four-topic form.
func TestDecode_unwrapReusesWrapBody(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	ev.Topic = []string{TopicSymbolUnwrap, ev.Topic[1], ev.Topic[2], ev.Topic[2]}
	got := mustDecodeOne(t, ev)
	if got.Kind != EventUnwrap || got.Owner != got.Caller || got.Shares.String() != "1000000000000000000" {
		t.Errorf("unwrap = %+v", got)
	}
	ev.Topic = ev.Topic[:3]
	if _, err := NewDecoder().Decode(ev); err == nil {
		t.Error("unwrap with 3 topics must error")
	}
}

// approve is recognised on both the wrapper and a YT and projects zero rows.
func TestGolden_approveRecognisedZeroRows(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ hash, name, id string }{
		{wrapperHash, "approve_ibt_64716538_2c89849ef438_CAHPZL.json", wrapperID},
		{ytHash, "approve_yt_64716590_91609b68ae79_CC3MKD.json", ytID},
	} {
		ev := loadFixture(t, c.hash, c.name)
		d := NewDecoder()
		if ev.ContractID != c.id || !d.Matches(ev) {
			t.Fatalf("%s: not claimed", c.name)
		}
		out, err := d.Decode(ev)
		if err != nil || len(out) != 0 {
			t.Errorf("%s: Decode = %v, %v; want no rows, no error", c.name, out, err)
		}
	}
}

// Same topic from a contract outside the curated set is not claimed (ADR-0035).
func TestMatches_RejectsSameTopicFromForeignContract(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	ev.ContractID = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	if NewDecoder().Matches(ev) {
		t.Error("foreign contract's wrap must not match")
	}
}

func TestMatches_RejectsUnclaimedTopicFromGatedContract(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	ev.Topic = []string{"AAAADwAAAAh1bmtub3duMQ=="}
	d := NewDecoder()
	if d.Matches(ev) {
		t.Error("unclaimed topic must not match")
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("Decode of an unclaimed topic must error")
	}
}

func TestDecode_MalformedBodyIsAnError(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	approve := loadFixture(t, wrapperHash, "approve_ibt_64716538_2c89849ef438_CAHPZL.json")
	ev.Value = approve.Value // Map without shares/vault_shares
	if _, err := NewDecoder().Decode(ev); err == nil {
		t.Error("wrap with a foreign body must error")
	}
}

func TestMainnetContracts_Shape(t *testing.T) {
	t.Parallel()
	if n := len(MainnetContracts); n != 18 {
		t.Fatalf("gated contracts = %d, want 18", n)
	}
	counts := map[Role]int{}
	for _, m := range MainnetContracts {
		counts[m.Role]++
	}
	if counts[RoleRegistry] != 1 || counts[RolePT] != 7 || counts[RoleYT] != 7 || counts[RoleIBT] != 3 {
		t.Errorf("role counts = %v", counts)
	}
	if got := NewDecoder().GatedContractSet(); len(got) != 18 {
		t.Errorf("GatedContractSet = %d, want 18", len(got))
	}
}
