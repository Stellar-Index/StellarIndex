package aquarius

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const routerUpgradeWasmHash = "2f8e7d7f237eca17e188c4e5d08b73b0065584b3d39f9e1d801993a8510a3107"

type routerFixture struct {
	ContractID     string   `json:"contract_id"`
	WasmHash       string   `json:"wasm_hash"`
	Ledger         uint32   `json:"ledger"`
	TxHash         string   `json:"tx_hash"`
	OpIndex        int      `json:"op_index"`
	EventIndex     int      `json:"event_index"`
	LedgerClosedAt string   `json:"ledger_closed_at"`
	Topics         []string `json:"topics"`
	Value          string   `json:"value"`
	EventName      string   `json:"event_name"`
}

func loadRouterFixture(t *testing.T, name string) (routerFixture, events.Event, time.Time) {
	t.Helper()
	path := filepath.Join("..", "..", "..", "test", "fixtures", "aquarius", routerUpgradeWasmHash, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fx routerFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if fx.WasmHash != routerUpgradeWasmHash {
		t.Fatalf("fixture wasm_hash = %s", fx.WasmHash)
	}
	for _, s := range append([]string{fx.Value}, fx.Topics...) {
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil || base64.StdEncoding.EncodeToString(b) != s {
			t.Fatalf("fixture base64 does not round-trip: %q", s)
		}
	}
	closedAt, err := time.Parse(time.RFC3339, fx.LedgerClosedAt)
	if err != nil {
		t.Fatal(err)
	}
	e := events.Event{
		Type:           "contract",
		ContractID:     fx.ContractID,
		Ledger:         fx.Ledger,
		TxHash:         fx.TxHash,
		OperationIndex: fx.OpIndex,
		EventIndex:     fx.EventIndex,
		LedgerClosedAt: fx.LedgerClosedAt,
		Topic:          fx.Topics,
		Value:          fx.Value,
	}
	return fx, e, closedAt
}

// The expected strkeys and integers below are hand-decoded from the raw XDR bytes (address = type 0x12, kind 1,
// 32 bytes; u128 = type 9; u64 = type 5), not read back from the decoder; the byte offsets are in the audit doc.
func TestRouterUpgradeFixtures_addPool(t *testing.T) {
	cases := []struct {
		file string
		pool string
	}{
		{"add_pool_65675280_bea153581781.json", "CBQ6AGQYE5HCDK7VIDUH6756IFPBTLF2HNQH57DAFMOHK3CVDH4CAMXO"},
		{"add_pool_65675307_a43b2fa30ab8.json", "CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI"},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			fx, e, _ := loadRouterFixture(t, c.file)
			if fx.ContractID != MainnetRouter {
				t.Fatalf("fixture emitter = %s", fx.ContractID)
			}
			if !isAddPool(&e) {
				t.Fatal("topic[0] is not the add_pool symbol")
			}
			got, poolType, err := decodeAnnouncedPool(&e)
			if err != nil {
				t.Fatal(err)
			}
			if got != c.pool {
				t.Errorf("announced pool = %s, want %s", got, c.pool)
			}
			// body[1] is Symbol("concentrated"): type 0x0f, length 12, at offsets 52-71 (audit doc byte table).
			if poolType != "concentrated" {
				t.Errorf("announced pool type = %q, want concentrated", poolType)
			}

			// Router trust root only: NewDecoder's in-code list already holds these pools.
			d := &Decoder{reg: contractid.New(contractid.WithFactories([]string{MainnetRouter}))}
			trade := events.Event{ContractID: c.pool, Topic: []string{TopicSymbolTrade, "x", "y", "z"}}
			if d.Matches(trade) {
				t.Fatal("pool matched before its add_pool")
			}
			if !d.Matches(e) {
				t.Fatal("router add_pool not matched")
			}
			if out, err := d.Decode(e); err != nil || len(out) != 0 {
				t.Fatalf("Decode(add_pool) = %v, %v; want no events, nil", out, err)
			}
			if !d.Matches(trade) {
				t.Error("announced pool not admitted by the gate")
			}
			if got := d.reg.AttrsOf(c.pool)[AttrLiquidityLayout]; got != LayoutConcentrated {
				t.Errorf("announced pool layout = %q, want %q", got, LayoutConcentrated)
			}
			if !slices.Contains(MainnetConcentratedPools, c.pool) {
				t.Errorf("%s is not in MainnetConcentratedPools; its layout would be lost on restart", c.pool)
			}
		})
	}
}

func rewardsTxHash(t *testing.T, file string) string {
	t.Helper()
	fx, _, _ := loadRouterFixture(t, file)
	return fx.TxHash
}

func TestRouterUpgradeFixtures_configRewards(t *testing.T) {
	cases := []struct {
		file     string
		poolFile string
		pool     string
		amount   string
		expires  uint64
	}{
		{"config_rewards_65667281_9ec5f48b4c10_3.json", "set_rewards_config_65667281_9ec5f48b4c10_2.json", "CDRRS2SLIP7THFGUZVB4QL2RAM2ZHSDMAD2GL5GEXTKK5AY3DQUX3Y7N", "3316077", 1795666520},
		{"config_rewards_65667281_9ec5f48b4c10_1.json", "set_rewards_config_65667281_9ec5f48b4c10_0.json", "CAAJYIESEIKI7KZV67MBSXPCRGKH5WY354WUS55H4UNBD35OSKYPZA7Q", "3831913", 1795666520},
	}
	for _, c := range cases {
		t.Run(c.file, func(t *testing.T) {
			_, e, closedAt := loadRouterFixture(t, c.file)
			if got := classify(&e); got != EventConfigRewards {
				t.Fatalf("classify = %q", got)
			}
			if !NewDecoder().Matches(e) {
				t.Fatal("router config_rewards not matched by the gate")
			}
			rv, err := decodeRewardsEvent(&e, EventConfigRewards, closedAt)
			if err != nil {
				t.Fatal(err)
			}
			if rv.Amount == nil || rv.Amount.String() != c.amount {
				t.Errorf("amount = %v, want %s", rv.Amount, c.amount)
			}
			if got := rv.Attributes["expires_at"]; got != c.expires {
				t.Errorf("expires_at = %v, want %d", got, c.expires)
			}
			if got := rv.Attributes["pool"]; got != c.pool {
				t.Errorf("pool = %v, want %s", got, c.pool)
			}
			refs, ok := rv.Attributes["refs"].([]string)
			if !ok || len(refs) != 2 || refs[1] != MainnetXLMSAC {
				t.Errorf("refs = %v, want 2 entries ending in the XLM SAC", rv.Attributes["refs"])
			}

			// The pool's own set_rewards_config in the same tx is emitted by a different contract (the pool, not the
			// router) and carries [expires_at: u64, amount: u128]; the router's config_rewards must agree with it.
			pfx, pe, _ := loadRouterFixture(t, c.poolFile)
			if pfx.ContractID != c.pool || pfx.TxHash != rewardsTxHash(t, c.file) {
				t.Fatalf("pool fixture %s is not the same-tx event of pool %s", c.poolFile, c.pool)
			}
			if got := classify(&pe); got != EventSetRewardsConfig {
				t.Fatalf("pool event classify = %q", got)
			}
			pv, err := decodeRewardsEvent(&pe, EventSetRewardsConfig, closedAt)
			if err != nil {
				t.Fatal(err)
			}
			if pv.Amount == nil || pv.Amount.String() != c.amount || pv.Attributes["expires_at"] != c.expires {
				t.Errorf("pool event amount/expires_at = %v/%v, want %s/%d", pv.Amount, pv.Attributes["expires_at"], c.amount, c.expires)
			}
			if pv.ContractID != rv.Attributes["pool"] {
				t.Errorf("pool event emitter %s != router event pool %v", pv.ContractID, rv.Attributes["pool"])
			}
			if pv.Amount.String() != rv.Amount.String() || pv.Attributes["expires_at"] != rv.Attributes["expires_at"] {
				t.Errorf("router (%v, %v) != pool (%v, %v)", rv.Amount, rv.Attributes["expires_at"], pv.Amount, pv.Attributes["expires_at"])
			}
		})
	}
}
