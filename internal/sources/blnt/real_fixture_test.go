package blnt

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

const fixtureRoot = "../../../test/fixtures/blnt"

type realFixture struct {
	ContractID     string   `json:"contract_id"`
	Role           Role     `json:"role"`
	WASMHash       string   `json:"wasm_hash"`
	Ledger         uint32   `json:"ledger"`
	TxHash         string   `json:"tx_hash"`
	OpIndex        int      `json:"op_index"`
	EventIndex     int      `json:"event_index"`
	LedgerClosedAt string   `json:"ledger_closed_at"`
	Topics         []string `json:"topics"`
	Value          string   `json:"value"`
	EventName      string   `json:"event_name"`
}

func (f realFixture) event() events.Event {
	return events.Event{
		Type: "contract", ContractID: f.ContractID, Ledger: f.Ledger,
		LedgerClosedAt: f.LedgerClosedAt, TxHash: f.TxHash,
		OperationIndex: f.OpIndex, EventIndex: f.EventIndex,
		Topic: f.Topics, Value: f.Value,
		InSuccessfulContractCall: true,
	}
}

func readRealFixture(t *testing.T, path string) realFixture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f realFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return f
}

// These records were exported from the public Galexie archive, not synthesized.
// Every immutable BLNT V3 role and audited WASM lineage must remain represented.
func TestRealFixturesDecodeEveryAuditedRole(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(fixtureRoot, "*", "*.json"))
	if err != nil || len(files) < 33 {
		t.Fatalf("fixtures = %d (err %v), want at least 33 lake records", len(files), err)
	}
	decoded := make(map[string]decodedRow, len(files))
	covered := map[Role]map[string]bool{}
	wantHashes := map[string]Role{
		"4fa7d99dee6fe99cad5d07a46ee39e437dc287e53e3ab1f3ccc0aacf7e6cd6c5": RoleEmitter,
		"dcdc0b1860799629c7c88804955dd7373cb6febaf069e2ba863e98ebbaa95c27": RoleBackfill,
		"985d7eda4b610f28103edcb28faa52408169f455ac9a880c4a39f58ecc0c2135": RoleFactory,
		"0469cfb445bedd87478274f265c8776b32cde1e8840fca4684ffa1dc4d5a750f": RoleBackstop,
		"0e4f8f45b94e6b0d017a496ac5620ad0e3f866533cf45acd2b070bedda1e7356": RolePool,
		"7c3d3591b6227827c21a0fe0da5df402bd55394fa5ed38c0bbace0400a823ddb": RoleLockupBackfill,
	}
	seen := make(map[string]bool, len(wantHashes))
	for _, path := range files {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			f := readRealFixture(t, path)
			hash := filepath.Base(filepath.Dir(path))
			wantRole, known := wantHashes[hash]
			if !known || f.WASMHash != hash || f.Role != wantRole {
				t.Fatalf("fixture path/hash/role mismatch: path=%s hash=%s role=%s", path, f.WASMHash, f.Role)
			}
			seen[hash] = true
			if !strings.HasPrefix(filepath.Base(path), f.EventName+"_") || len(f.Topics) == 0 ||
				f.Topics[0] != scval.MustEncodeSymbol(f.EventName) {
				t.Fatalf("event name/topic/path mismatch for %s", path)
			}
			for _, raw := range append([]string{f.Value}, f.Topics...) {
				if _, err := base64.StdEncoding.DecodeString(raw); err != nil {
					t.Fatalf("invalid base64 XDR: %v", err)
				}
			}

			d := NewDecoder()
			ev := f.event()
			if !d.Matches(ev) {
				t.Fatalf("decoder did not claim %s/%s", f.Role, f.EventName)
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("Decode emitted %d events, want 1", len(out))
			}
			got := out[0].(Event)
			if got.Role != f.Role || got.EventType != f.EventName || got.ContractID != f.ContractID ||
				got.Ledger != f.Ledger || got.TxHash != f.TxHash || int(got.OpIndex) != f.OpIndex ||
				int(got.EventIndex) != f.EventIndex {
				t.Fatalf("decoded envelope differs from the lake fixture: %+v", got)
			}
			decoded[filepath.Base(path)] = decodedRow{
				Pool: got.Pool, User: got.UserAddress, Asset: got.Asset, Tier: got.Tier,
				Amount: got.Amount, Amount2: got.Amount2, Attributes: got.Attributes,
			}
			if covered[f.Role] == nil {
				covered[f.Role] = map[string]bool{}
			}
			covered[f.Role][f.EventName] = true
			if got.ObservedAt.UTC().Format("2006-01-02T15:04:05Z") != f.LedgerClosedAt {
				t.Fatalf("close time = %s, want %s", got.ObservedAt, f.LedgerClosedAt)
			}
		})
	}
	for hash, role := range wantHashes {
		if !seen[hash] {
			t.Errorf("no real fixture for %s WASM %s", role, hash)
		}
	}
	checkGolden(t, decoded)
	checkCoverage(t, covered)
}

// decodedRow is the typed payload the golden file pins for each real event.
type decodedRow struct {
	Pool       string         `json:"pool,omitempty"`
	User       string         `json:"user_address,omitempty"`
	Asset      string         `json:"asset,omitempty"`
	Tier       string         `json:"tier,omitempty"`
	Amount     string         `json:"amount,omitempty"`
	Amount2    string         `json:"amount2,omitempty"`
	Attributes map[string]any `json:"attributes"`
}

const goldenPath = "testdata/decoded.golden.json"

// checkGolden pins what every real event decodes to. Regenerate with UPDATE_GOLDEN=1 and read the diff.
func checkGolden(t *testing.T, decoded map[string]decodedRow) {
	t.Helper()
	got, err := json.MarshalIndent(decoded, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(goldenPath, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("decoded rows differ from %s; rerun with UPDATE_GOLDEN=1 and review the diff", goldenPath)
	}
}

// sourceOnly lists the ABI kinds no deployed contract had emitted when the fixtures were cut
// (lake read at ledger 65,512,176). Their specs come from the contract source alone. A kind
// leaves this list the moment a real fixture for it is added.
var sourceOnly = map[Role][]string{
	RolePool: {
		"update_pool", "cancel_set_reserve", "bad_debt", "defaulted_debt", "supply", "withdraw",
		"clawback", "gulp", "reconcile_loss", "protocol_credit_loss", "delete_auction",
	},
	RoleBackstop:       {"rw_zone_remove", "draw"},
	RoleEmitter:        {"q_swap", "del_swap", "swap"},
	RoleBackfill:       {"burn_expired"},
	RoleLockupBackfill: {"claim", "expire"},
}

// checkCoverage requires every ABI kind to be either proven on real bytes or listed as source-only.
func checkCoverage(t *testing.T, covered map[Role]map[string]bool) {
	t.Helper()
	for role, kinds := range abi {
		for kind := range kinds {
			real, listed := covered[role][kind], slices.Contains(sourceOnly[role], kind)
			switch {
			case real && listed:
				t.Errorf("%s/%s has a real fixture; remove it from sourceOnly", role, kind)
			case !real && !listed:
				t.Errorf("%s/%s has neither a real fixture nor a sourceOnly entry", role, kind)
			}
		}
	}
	for role, kinds := range sourceOnly {
		for _, kind := range kinds {
			if _, ok := abi[role][kind]; !ok {
				t.Errorf("sourceOnly names %s/%s, which the ABI does not declare", role, kind)
			}
		}
	}
}
