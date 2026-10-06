package spectra

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

const fixtureRoot = "../../../test/fixtures/spectra"

// Mainnet fixtures: lake exports and scripts/dev/capture-spectra-fixtures.sh.
const (
	wrapperHash = "0e8d68e206d66b087a2e0b64430356aec834ff906260e057180702d74d6f81f4"
	blendHash   = "11b4bacca38f670e792b7a1bbd6c6888f9e572c06e1def9cc5adf9294403bbb2"
	routerHash  = "1452052c3c9ed3297b82a02e99baafb0c9ba5209f03ad941e6427c3aae5b07f7"
	engineHash  = "20ec1744c3f9e2c840c714b4ad2dc42d844d2eb7c164ced8c685cf267516b0f0"
	ptHash      = "3bcf316f76a9c5db718ccd6e2292cad70d8e8ce4371dfa0a2a7fdd86b8749ffd"
	registryHsh = "54c79aac3d2a971fb8d11e7cbbab87ee2a4be10b60b48bb21e1d6254affe5bf3"
	factoryHash = "763ea32c344f79bbce87d814f55b3328b831237443084e726caf8b8e4ac2b4a7"
	ytHash      = "daeb931b9507440b6dc4f73256ac33c6089964f5d035f7eec5fcedc10e732c8e"

	wrapperID  = "CAHPZLEH6O6WJPICJAVRLCTYCAYDN52F4SM6IX3XJKWYSAASSHGZEZBO"
	ytID       = "CC3MKDR62O4Q7EEBOXXCNFJH3Z6AZQLB3QUTUEKAMSIJIYAHLUPVQR5G"
	registryID = "CCUGRASBWD5SXDYMS7NM437FQ7KNKHFX74D2VRJVTRU4J2TWDMURUW3V"
	foreignID  = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"

	admin      = "GCNC7GXVI2LYUNS7VMJXRDC57HV2LPX2PXXTSVQIJ6NFH5QQWNNIUUC7"
	engine     = "CCKNOCLH6QILGS6GYZWMQ6JCHWC2D75OCI5RLBPCUF7FJTONNSCZZAC5"
	usdcPT     = "CAAOR5F43GSQZYJESHIVLGZBMHMH3UVJMSBEHFKCUBKOUQSZMQC5UCMK"
	usdcYT     = "CBDQZFWY735RH3PQLNK4BIOO7DTJY4ZQWK5YDFCZ7EIL5DAO2567TORX"
	usdcIBT    = "CBRT4E5AH23GMRQI7H6HQW54HMDMK4C23OO2CEN5OHWEOSRYBQZCMYBC"
	earnUSDCPT = "CCJ43PIDUBSVX4FAI3MJJTFWC3ZXLECARFNJUOUUTKHB5JP32Q75LVPN"
	earnXLMPT  = "CD5YZRFQCATFOFZPWE4XDYJZMXAZSW5ROTIAO4D65Q7KTMZIEWGB7H7W"
)

type fixture struct {
	ContractID     string   `json:"contract_id"`
	Role           Role     `json:"role"`
	Ledger         uint32   `json:"ledger"`
	TxHash         string   `json:"tx_hash"`
	OpIndex        int      `json:"op_index"`
	EventIndex     int      `json:"event_index"`
	LedgerClosedAt string   `json:"ledger_closed_at"`
	Topics         []string `json:"topics"`
	Value          string   `json:"value"`
	EventName      string   `json:"event_name"`
}

func readFixture(t *testing.T, path string) fixture {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var f fixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("parse fixture %s: %v", path, err)
	}
	return f
}

func (f fixture) event() events.Event {
	return events.Event{
		Type: "contract", ContractID: f.ContractID, Ledger: f.Ledger,
		LedgerClosedAt: f.LedgerClosedAt, TxHash: f.TxHash,
		OperationIndex: f.OpIndex, EventIndex: f.EventIndex,
		Topic: f.Topics, Value: f.Value,
	}
}

func loadFixture(t *testing.T, hash, name string) events.Event {
	t.Helper()
	return readFixture(t, filepath.Join(fixtureRoot, hash, name)).event()
}

func mustDecodeOne(t *testing.T, ev events.Event) Event {
	t.Helper()
	return mustDecodeOneWith(t, NewDecoder(), ev)
}

func mustDecodeOneWith(t *testing.T, d *Decoder, ev events.Event) Event {
	t.Helper()
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

// row is an Event's non-identity fields, amounts as decimal strings.
type row struct {
	Kind                                                   string
	Role                                                   Role
	Market, Caller, Receiver, Owner, Maker, Order, IBT, YT string
	Duration                                               uint64
	Shares, VaultShares, Assets, Amount, Yield             string
}

func rowOf(e Event) row {
	return row{
		e.Kind, e.Role, e.MarketPT, e.Caller, e.Receiver, e.Owner, e.Maker, e.OrderID, e.IBT, e.YT,
		e.DurationSeconds, e.Shares.String(), e.VaultShares.String(), e.Assets.String(), e.Amount.String(),
		e.YieldInIBT.String(),
	}
}

// goldens pins every row-kind fixture's decoded values, at least one per
// (WASM hash, kind). Unset amounts render "0".
var goldens = map[string]row{
	// factory: market announcements, three durations
	factoryHash + "/pt_deployed_factory_63782624_dc730b2a132e_6_CC4ZVR.json": {
		Kind: EventPTDeployed, Role: RoleFactory, Market: usdcPT, Caller: admin, IBT: usdcIBT, Duration: 7_776_000,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	factoryHash + "/pt_deployed_factory_64453034_e896e7e581db_6_CC4ZVR.json": {
		Kind: EventPTDeployed, Role: RoleFactory, Market: "CDRK5SWZ7DQJ4BZUAZQABPSP7MZS4NO4PPW7LW6CVD5THZDVE63MVLJJ",
		Caller: admin, IBT: wrapperID, Duration: 2_592_000,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	factoryHash + "/pt_deployed_factory_64453038_8f0b4559a79b_6_CC4ZVR.json": {
		Kind: EventPTDeployed, Role: RoleFactory, Market: "CDHIBKKS53XQAMIDVL7SZLPM2DEHH3OCO7SU6IE3OW65LESYF5K5ABMU",
		Caller: admin, IBT: wrapperID, Duration: 15_552_000,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	// registry
	registryHsh + "/pt_added_registry_63782624_dc730b2a132e_5_CCUGRA.json": {
		Kind: EventPTAdded, Role: RoleRegistry, Market: usdcPT,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	// PT
	ptHash + "/yt_deployed_pt_63782624_dc730b2a132e_4_CAAOR5.json": {
		Kind: EventYTDeployed, Role: RolePT, Market: usdcPT, YT: usdcYT,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/pt_minted_pt_63783310_40fa3a0345a9_2_CAAOR5.json": {
		Kind: EventPTMinted, Role: RolePT, Market: usdcPT, Caller: admin, Receiver: admin,
		Shares: "568278", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/pt_minted_pt_63812811_f2b685315023_2_CCJ43P.json": {
		Kind: EventPTMinted, Role: RolePT, Market: earnUSDCPT, Caller: admin, Receiver: admin,
		Shares: "99999998154793", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/pt_minted_pt_64347726_c94f952f9867_20_CAAOR5.json": {
		Kind: EventPTMinted, Role: RolePT, Market: usdcPT, Caller: engine, Receiver: engine,
		Shares: "380400792", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/redeem_pt_63812795_6dbb49828778_3_CD5YZR.json": {
		Kind: EventRedeem, Role: RolePT, Market: earnXLMPT, Owner: admin, Receiver: admin,
		Shares: "10000000000000", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/redeem_pt_63812816_2a008c6241e0_3_CCJ43P.json": {
		Kind: EventRedeem, Role: RolePT, Market: earnUSDCPT, Owner: admin, Receiver: admin,
		Shares: "10000000000000", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	ptHash + "/yield_updated_pt_63783681_a52383fad529_1_CAAOR5.json": {
		Kind: EventYieldUpdated, Role: RolePT, Market: usdcPT, Owner: admin,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "2",
	},
	ptHash + "/transfer_pt_63784450_db4f7aa725b8_1_CAAOR5.json": {
		Kind: EventTransfer, Role: RolePT, Market: usdcPT, Caller: admin, Receiver: engine,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "40000", Yield: "0",
	},
	// YT: the market is the YT's PT
	ytHash + "/transfer_yt_63784324_2b7c7486b916_2_CBDQZF.json": {
		Kind: EventTransfer, Role: RoleYT, Market: usdcPT, Caller: admin, Receiver: engine,
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "40000", Yield: "0",
	},
	// order engine
	engineHash + "/order_registered_order_engine_63783657_98459b3ea469_0_CCKNOC.json": {
		Kind: EventOrderRegistered, Role: RoleOrderEngine, Maker: admin,
		Order:  "758d8bb1423990c92e01edc4390e65535aeb54457647d00d45ac61095049c851",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "50000", Yield: "0",
	},
	engineHash + "/order_registered_order_engine_64347726_c94f952f9867_4_CCKNOC.json": {
		Kind: EventOrderRegistered, Role: RoleOrderEngine, Maker: "GCMFDGOMTOHAV5KOF645AT2WRL3EVHPAWWX37WPRB5D7UD663I72LKVF",
		Order:  "2ed916b579b1ebea4de168370fcf5c381efd12978b662dd5d8461cae303f0c25",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "100000000", Yield: "0",
	},
	engineHash + "/order_filled_order_engine_63784324_2b7c7486b916_4_CCKNOC.json": {
		Kind: EventOrderFilled, Role: RoleOrderEngine,
		Order:  "0cff695f9ef9989e1809e843a0548ce3e71223438156abfa777ef23ed0c643e5",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "40000", Yield: "0",
	},
	engineHash + "/order_filled_order_engine_64347726_c94f952f9867_23_CCKNOC.json": {
		Kind: EventOrderFilled, Role: RoleOrderEngine,
		Order:  "4ca542e184808531705977f996b247627c42e0b6284587c1e43c87bc0f3f0b8a",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "275966262", Yield: "0",
	},
	engineHash + "/order_cancelled_order_engine_63798657_271e0e723e4e_0_CCKNOC.json": {
		Kind: EventOrderCancelled, Role: RoleOrderEngine, Maker: "GA6PLM43TPTJLPCCW5YX3VH6U43JKCFTDETPDZVHCMOHOTV23EOIFIFO",
		Order:  "d566dc8983e4b1daa082b7e02a11d39c4bdec0be410e183f4310d39e652e277a",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	engineHash + "/order_cancelled_order_engine_64347750_6ea1aaca35d6_0_CCKNOC.json": {
		Kind: EventOrderCancelled, Role: RoleOrderEngine, Maker: "GD6B32OSXHACMKOMAS5FW2FFEKQ2WZ2YB22S23YQKTRPR4JDAUFYBW5T",
		Order:  "ee82858af1b01185bb5e97e8e43a48ee1583e500a4d4031c6081844b2b27401e",
		Shares: "0", VaultShares: "0", Assets: "0", Amount: "0", Yield: "0",
	},
	// Blend wrapper (ERC-4626 deposit / withdraw)
	blendHash + "/deposit_ibt_63782614_5eaeea223b42_3_CBRT4E.json": {
		Kind: EventDeposit, Role: RoleIBT, Caller: admin, Receiver: admin, Owner: admin,
		Shares: "879858", VaultShares: "0", Assets: "1000000", Amount: "0", Yield: "0",
	},
	blendHash + "/deposit_ibt_64270260_1ffd3ba58efb_3_CBRT4E.json": {
		Kind: EventDeposit, Role: RoleIBT, Caller: "GD6B32OSXHACMKOMAS5FW2FFEKQ2WZ2YB22S23YQKTRPR4JDAUFYBW5T",
		Receiver: "GD6B32OSXHACMKOMAS5FW2FFEKQ2WZ2YB22S23YQKTRPR4JDAUFYBW5T",
		Owner:    "GD6B32OSXHACMKOMAS5FW2FFEKQ2WZ2YB22S23YQKTRPR4JDAUFYBW5T",
		Shares:   "8744459", VaultShares: "0", Assets: "10000000", Amount: "0", Yield: "0",
	},
	blendHash + "/withdraw_ibt_63785018_16be0ee7541c_2_CBRT4E.json": {
		Kind: EventWithdraw, Role: RoleIBT, Caller: admin, Receiver: admin, Owner: admin,
		Shares: "20000", VaultShares: "0", Assets: "22731", Amount: "0", Yield: "0",
	},
	// the one withdraw whose three topic addresses differ: pins their order
	blendHash + "/withdraw_ibt_64270260_1ffd3ba58efb_8_CBRT4E.json": {
		Kind: EventWithdraw, Role: RoleIBT, Caller: engine,
		Receiver: "GCMFDGOMTOHAV5KOF645AT2WRL3EVHPAWWX37WPRB5D7UD663I72LKVF", Owner: engine,
		Shares: "8744407", VaultShares: "0", Assets: "9999939", Amount: "0", Yield: "0",
	},
	// deJTRSY wrapper
	wrapperHash + "/wrap_ibt_64453013_a973b0ee7a9e_1_CAHPZL.json": {
		Kind: EventWrap, Role: RoleIBT, Caller: admin, Receiver: admin,
		Shares: "3000000000000000000", VaultShares: "3000000000000000000", Assets: "0", Amount: "0", Yield: "0",
	},
	wrapperHash + "/wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json": {
		Kind: EventWrap, Role: RoleIBT,
		Caller:   "GAHWFCCYCDSSEYZRVV4446KBDP6X2JR2T56TG4OGANS3AS3NCILOCGU5",
		Receiver: "GAHWFCCYCDSSEYZRVV4446KBDP6X2JR2T56TG4OGANS3AS3NCILOCGU5",
		Shares:   "1000000000000000000", VaultShares: "1000000000000000000", Assets: "0", Amount: "0", Yield: "0",
	},
}

// Every lake fixture is claimed and decodes; a row kind yields exactly its
// golden, every other kind zero rows and no error.
func TestGolden_EveryFixture(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join(fixtureRoot, "*", "*.json"))
	if err != nil || len(files) < 70 {
		t.Fatalf("fixtures = %d (err %v), want the full lake set", len(files), err)
	}
	seen := map[string]bool{}
	for _, path := range files {
		key := filepath.Base(filepath.Dir(path)) + "/" + filepath.Base(path)
		t.Run(key, func(t *testing.T) {
			f := readFixture(t, path)
			ev := f.event()
			d := NewDecoder()
			if !d.Matches(ev) {
				t.Fatalf("%s from %s (%s) not claimed", f.EventName, f.ContractID, f.Role)
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			want, isRow := goldens[key]
			if !isRow {
				if len(out) != 0 {
					t.Fatalf("%s: %d rows, want 0 (no golden for it)", f.EventName, len(out))
				}
				return
			}
			seen[key] = true
			if len(out) != 1 {
				t.Fatalf("%d rows, want 1", len(out))
			}
			got := out[0].(Event)
			if g := rowOf(got); g != want {
				t.Errorf("decoded\n got %+v\nwant %+v", g, want)
			}
			if got.Kind != f.EventName || got.Role != f.Role {
				t.Errorf("kind/role = %s/%s, fixture says %s/%s", got.Kind, got.Role, f.EventName, f.Role)
			}
			at := got.ObservedAt.UTC().Format("2006-01-02T15:04:05Z")
			if got.ContractID != f.ContractID || got.Ledger != f.Ledger || got.TxHash != f.TxHash ||
				int(got.OpIndex) != f.OpIndex || int(got.EventIndex) != f.EventIndex || at != f.LedgerClosedAt {
				t.Errorf("identity = %s %d %s %d/%d %s", got.ContractID, got.Ledger, got.TxHash, got.OpIndex, got.EventIndex, at)
			}
			if got.EventKind() != EventKind || got.Source() != SourceName {
				t.Errorf("consumer identity = %q %q", got.EventKind(), got.Source())
			}
		})
	}
	t.Cleanup(func() {
		for key := range goldens {
			if !seen[key] {
				t.Errorf("golden %s matched no fixture", key)
			}
		}
	})
}

// Same topic from a contract outside the gate is not claimed (ADR-0035).
// role_granted is also emitted by unrelated contracts with another body.
func TestMatches_RejectsSameTopicFromForeignContract(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ hash, name string }{
		{wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json"},
		{registryHsh, "role_granted_registry_63778088_dc1ca77c1342_0_CCUGRA.json"},
		{engineHash, "order_filled_order_engine_63784324_2b7c7486b916_4_CCKNOC.json"},
		{ptHash, "transfer_pt_63784450_db4f7aa725b8_1_CAAOR5.json"},
	} {
		ev := loadFixture(t, c.hash, c.name)
		ev.ContractID = foreignID
		d := NewDecoder()
		if d.Matches(ev) {
			t.Errorf("%s: foreign contract matched", c.name)
		}
		if _, err := d.Decode(ev); err == nil {
			t.Errorf("%s: foreign contract decoded", c.name)
		}
	}
}

// A kind is claimed only from the role observed emitting it.
func TestMatches_KindFromWrongRoleNotClaimed(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ hash, name, from string }{
		{factoryHash, "pt_deployed_factory_63782624_dc730b2a132e_6_CC4ZVR.json", registryID},
		{factoryHash, "pt_deployed_factory_63782624_dc730b2a132e_6_CC4ZVR.json", usdcPT},
		{ptHash, "yt_deployed_pt_63782624_dc730b2a132e_4_CAAOR5.json", usdcYT},
		{ptHash, "pt_minted_pt_63783310_40fa3a0345a9_2_CAAOR5.json", usdcYT},
		{registryHsh, "pt_added_registry_63782624_dc730b2a132e_5_CCUGRA.json", MainnetFactory},
		{engineHash, "order_registered_order_engine_63783657_98459b3ea469_0_CCKNOC.json", usdcPT},
		{registryHsh, "factory_change_registry_63778155_79683f438c5d_0_CCUGRA.json", engine},
		{ptHash, "role_granted_pt_63782624_dc730b2a132e_0_CAAOR5.json", usdcYT},
	} {
		ev := loadFixture(t, c.hash, c.name)
		ev.ContractID = c.from
		if NewDecoder().Matches(ev) {
			t.Errorf("%s claimed from %s", classify(&ev), c.from)
		}
	}
}

func TestMatches_RejectsUnclaimedTopicFromGatedContract(t *testing.T) {
	t.Parallel()
	ev := loadFixture(t, wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json")
	ev.Topic = []string{sym("unknown1")}
	d := NewDecoder()
	if d.Matches(ev) {
		t.Error("unclaimed topic must not match")
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("Decode of an unclaimed topic must error")
	}
}

// A body that lacks a required field is an error, never a zero amount.
func TestDecode_MalformedBodyIsAnError(t *testing.T) {
	t.Parallel()
	// {new: address} carries none of any row kind's fields; approve's
	// {amount, live_until_ledger} has no `new`.
	foreign := loadFixture(t, registryHsh, "router_change_registry_63778178_df72289e24e9_0_CCUGRA.json").Value
	approve := loadFixture(t, wrapperHash, "approve_ibt_64716538_2c89849ef438_CAHPZL.json").Value
	for _, c := range []struct{ hash, name string }{
		{wrapperHash, "wrap_ibt_64716536_f7d5d536ff4d_CAHPZL.json"},
		{factoryHash, "pt_deployed_factory_63782624_dc730b2a132e_6_CC4ZVR.json"},
		{ptHash, "pt_minted_pt_63783310_40fa3a0345a9_2_CAAOR5.json"},
		{ptHash, "yield_updated_pt_63783681_a52383fad529_1_CAAOR5.json"},
		{ptHash, "transfer_pt_63784450_db4f7aa725b8_1_CAAOR5.json"},
		{engineHash, "order_filled_order_engine_63784324_2b7c7486b916_4_CCKNOC.json"},
		{engineHash, "order_cancelled_order_engine_63798657_271e0e723e4e_0_CCKNOC.json"},
		{blendHash, "deposit_ibt_63782614_5eaeea223b42_3_CBRT4E.json"},
		{registryHsh, "factory_change_registry_63778155_79683f438c5d_0_CCUGRA.json"},
	} {
		ev := loadFixture(t, c.hash, c.name)
		switch {
		case strings.HasPrefix(c.name, "factory_change"):
			ev.Value = approve
		case strings.HasPrefix(c.name, "order_cancelled"):
			ev.Value = loadFixture(t, ptHash, "transfer_pt_63784450_db4f7aa725b8_1_CAAOR5.json").Value // not a Map
		default:
			ev.Value = foreign
		}
		if out, err := NewDecoder().Decode(ev); err == nil {
			t.Errorf("%s with a foreign body decoded: %+v", c.name, out)
		}
	}
	short := loadFixture(t, engineHash, "order_registered_order_engine_63783657_98459b3ea469_0_CCKNOC.json")
	short.Topic = short.Topic[:2]
	if _, err := NewDecoder().Decode(short); err == nil {
		t.Error("order_registered without its order_id topic decoded")
	}
}

func TestMainnetContracts_Shape(t *testing.T) {
	t.Parallel()
	if n := len(MainnetContracts); n != 18 {
		t.Fatalf("gated contracts = %d, want 18", n)
	}
	counts := map[Role]int{}
	ytsPerPT := map[string]int{}
	for id, m := range MainnetContracts {
		counts[m.Role]++
		if m.Role == RoleYT {
			if MainnetContracts[m.MarketPT].Role != RolePT {
				t.Errorf("YT %s market %q is not a PT", id, m.MarketPT)
			}
			ytsPerPT[m.MarketPT]++
		} else if m.MarketPT != "" {
			t.Errorf("%s %s carries a market", m.Role, id)
		}
	}
	if counts[RoleRegistry] != 1 || counts[RolePT] != 7 || counts[RoleYT] != 7 || counts[RoleIBT] != 3 {
		t.Errorf("role counts = %v", counts)
	}
	for pt, n := range ytsPerPT {
		if n != 1 {
			t.Errorf("PT %s has %d YTs", pt, n)
		}
	}
	// 18 hand-kept + factory + router + 2 order engines
	if got := NewDecoder().GatedContractSet(); len(got) != 22 {
		t.Errorf("GatedContractSet = %d, want 22", len(got))
	}
	if fmt.Sprint(MainnetInfrastructure) != fmt.Sprint([]string{MainnetFactory, MainnetRouter, MainnetOrderEngines[0], MainnetOrderEngines[1]}) {
		t.Errorf("MainnetInfrastructure = %v", MainnetInfrastructure)
	}
}
