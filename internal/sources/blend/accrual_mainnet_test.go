package blend

import (
	"math/big"
	"testing"
)

// Consecutive stored ResData versions of one reserve (asset contract ADEFCE59…E496), read from
// stellar.ledger_entry_changes. Both pools' Config bstop_rate is 0_2000000; per-window inference
// from the data agrees.
type mainnetVersion struct {
	ledger uint32
	tx     string
	rd     rdIn // d_rate, b_rate, b_supply, d_supply, backstop_credit, last_time
}

// V2 pool CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD. Every version follows a supply
// or withdraw in the same tx (b_supply moves); the two at ledger 65641374 share a timestamp.
var mainnetV2Supplies = []mainnetVersion{
	{65641111, "d5c81788e7c591a2dd830e9983c7890670bb37eb099e1b87e37d398013f1f82f", rdIn{"1257757181499", "1160594019282", "341840600446096", "252833577392372", "1860223681272", 1795491604}},
	{65641173, "4927ea3a461620ec358af4d1393241298c9fb96040e7c6fa285067c4de20ee29", rdIn{"1257758339670", "1160594704571", "341839738818866", "252833577392372", "1860282246175", 1795491914}},
	{65641198, "b18e332d376ebc78864d4b60b90061e2dce20007a1df77c688cfcdaaf84642a3", rdIn{"1257758806687", "1160594980905", "341839205557900", "252833577392372", "1860305861690", 1795492039}},
	{65641212, "9b9f27d1110f12753446db7c7c02b18fa06697d6d873a809cad0145ab77f43d7", rdIn{"1257759068221", "1160595135655", "341839162476554", "252833577392372", "1860319086605", 1795492109}},
	{65641225, "15e76312ccdb5f8183ea07e8dfd71db525502b230e3334cd0998bd57d9e469ea", rdIn{"1257759311075", "1160595279352", "341838280515358", "252833577392372", "1860331366934", 1795492174}},
	{65641253, "80c37d74f97554b92338b73a6e9893a645cf05612aec466234aff748d6ab0324", rdIn{"1257759834156", "1160595588860", "341838323596686", "252833577392372", "1860357817422", 1795492314}},
	{65641260, "7f7435e7d9f1555bd81bdceb98e3a2c4f210ae8b64f56916cbe99bf470353e79", rdIn{"1257759964927", "1160595666237", "341838213480817", "252833577392372", "1860364430082", 1795492349}},
	{65641290, "f32ad7ba28e2d979b575883f6d6a0d648950710c9e4ac4b5b34db2c268443b26", rdIn{"1257760525374", "1160595997855", "341838213480814", "252833577392372", "1860392770046", 1795492499}},
	{65641339, "72573173405bcea251a734e94405976cfd0401fb13cd4815066cab7c742519ee", rdIn{"1257761440771", "1160596539498", "341838816618921", "252833577392372", "1860439058665", 1795492744}},
	{65641355, "b09148ea198295cc1f4090316a40387da283fd5529c502bb4840df3b772c583f", rdIn{"1257761739673", "1160596716359", "341838859700207", "252833577392372", "1860454173157", 1795492824}},
	{65641364, "22f6ff905c1e2fba8435d60040a3c1611640d09afe4be699fe9c533db596ff49", rdIn{"1257761907806", "1160596815843", "341838773537640", "252833577392372", "1860462675090", 1795492869}},
	{65641374, "7b9f3b593898fa308527710b771a733ed269442bc3f92a119ee03a0c7b6b5d06", rdIn{"1257762094621", "1160596926381", "341838773537639", "252833577392372", "1860472121711", 1795492919}},
	{65641374, "c927d11e6a69e980581b0197efed88abbb3191af7e3b009a4dac4605bb5a796d", rdIn{"1257762094621", "1160596926381", "341840927601589", "252833577392372", "1860472121711", 1795492919}},
	{65641393, "4abd5d792ba19603b4e16c78b65f2b6c875fcbcbc40282970e8666cf8def9ec8", rdIn{"1257762449548", "1160597136390", "341840755276504", "252833577392372", "1860490069203", 1795493014}},
}

// Same V2 reserve: three repays share ledger 65589583 and a fourth follows, so d_supply moves and
// each accrual must use the previous version's d_supply.
var mainnetV2Repays = []mainnetVersion{
	{65589475, "deafc756896287f7e9274ac5a045941c5ef4ed6a08245a31cede70d3e8ef78fa", rdIn{"1256798558179", "1160027128344", "342985389875351", "252952809659251", "1811749202329", 1795233421}},
	{65589486, "05668ec11ff2ec5de445a16c9dcdd14bfca189d1853322536a0387f041e2cb4a", rdIn{"1256798759742", "1160027247266", "342985467201120", "252952809659251", "1811759399514", 1795233476}},
	{65589491, "9ae6344e90bc5625b43a8e16be970fcfbd76e1b4a325b0f96f19f337a3e1e03d", rdIn{"1256798851362", "1160027301322", "342983743103766", "252952809659251", "1811764034621", 1795233501}},
	{65589495, "513d4e1ec8fee646f2890966df44ebebf51bc6ca96b6536c72838b12b02a114f", rdIn{"1256798924659", "1160027344567", "342983786206198", "252952809659251", "1811767742757", 1795233521}},
	{65589531, "9b91342e07f3555817fb690ef295ec88617f60434e33558ec767626df12646c4", rdIn{"1256799584319", "1160027733769", "342983786198080", "252952809659251", "1811801115327", 1795233701}},
	{65589581, "9919f963670bb1f1021693d9a20658fdb613e59b805b0180ef2faa30023b7326", rdIn{"1256800500514", "1160028274328", "342983786198080", "252952809659251", "1811847466147", 1795233951}},
	{65589583, "c926f721b283e96ea6fffa7c55ca96df5aa33032b1f0396e026725274ee26761", rdIn{"1256800537163", "1160028295951", "342983786198080", "252940401186679", "1811849320240", 1795233961}},
	{65589583, "d792a207c39e6d467999aa8beace2f0ba271babfe6937c85cf5e00d170ac544a", rdIn{"1256800537163", "1160028295951", "342983786198080", "252890203274911", "1811849320240", 1795233961}},
	{65589583, "4a6683544da918f55eb7f8ac89a40a6bb74e81291b2ba4abff237f3c73fdb0d0", rdIn{"1256800537163", "1160028295951", "342983786198080", "252840005363144", "1811849320240", 1795233961}},
	{65589591, "3e6058bb54917716211c205dfb553338760ecf7fbd7bb32807389dbdddf88e76", rdIn{"1256800683718", "1160028382380", "342976815026759", "252833577392416", "1811856731233", 1795234001}},
	{65589602, "ad891d9e69d6a3179dd07565e3b1cd4dc7c7d9296bb1b3986cf2bca72bde2ae2", rdIn{"1256800885230", "1160028501219", "342983888693543", "252833577392416", "1811866921033", 1795234056}},
	{65589619, "238d872f62cc96d04630b1e7794ea78b70bad96c7a5135ee1c44d0566922ebcf", rdIn{"1256801196653", "1160028684873", "342984492126896", "252833577392416", "1811882668671", 1795234141}},
	{65589632, "3caa1373a4b3eff1997899054087612651f2a5e2849574b113efe503cb935759", rdIn{"1256801434801", "1160028825315", "342979813536259", "252833577392416", "1811894711033", 1795234206}},
}

// V1 pool CDVQVKOY2YSXS2IC7KN6MNASSHPAO7UN2UR2ON4OI2SKMFJNVAMDX6DP: supplies, borrows and repays,
// and at 53013801 a backstop interest auction fill, which takes its lot out of backstop_credit
// (backstop_interest_auction.rs) and so is reported, not reconciled.
var mainnetV1 = []mainnetVersion{
	{53006991, "25e07fa929b2f9cda5b91ad283e54e206ebf3533a6929a08eb220d2403dc233e", rdIn{"1047159482", "1032178803", "18797888909879", "16194713774324", "2059091574", 1723519251}},
	{53007544, "fba400e2ac319a23202d647951558e6adec8a0be210419b99c7c2d23b3e7a79a", rdIn{"1047186292", "1032197280", "18797142928451", "16194713774324", "2145927629", 1723522377}},
	{53013465, "b8ec77dc3bd4bf748e20001f75f4997953dcce29a423ceeec016201a0b7fc207", rdIn{"1047476836", "1032397534", "18797142928451", "16195954851889", "3086983012", 1723556238}},
	{53013484, "9e75ee5ed3fa27b392fd78e92538db6503dce3c52ab9f19045757a677e9d7f90", rdIn{"1047477767", "1032398175", "18797191359377", "16195954851889", "3089998698", 1723556346}},
	{53013684, "b9265c69d93ab5885210d337787f9442e24889e6882bade0d3fc2096d6b58b80", rdIn{"1047487743", "1032405051", "18797191359377", "16194706130463", "3122312867", 1723557504}},
	{53013724, "803ce8f428b261f9f1caf57c58a4dc5ed3ac5b631870baa4e6faf285fae0b054", rdIn{"1047489708", "1032406405", "18797191359377", "16194412402028", "3128677386", 1723557732}},
	{53013801, "47ce695ff764ca7e03037f9792c1e08d5682170da67200bc901385edc1c0b174", rdIn{"1047493481", "1032409005", "18797191359377", "16194412402028", "44673266", 1723558170}},
	{53014563, "b3c2df2644bd971f7903a3a66cbf1b08083df20847314d1a93d9c284c00c2317", rdIn{"1047531658", "1032435317", "18804066741741", "16194412402028", "168324082", 1723562603}},
	{53014567, "ac3db46eb47f4e2a1e1e1d3d743ac1a3eecdee92b1946781d60bef34a785d6f3", rdIn{"1047531848", "1032435447", "18804066741741", "16208608033366", "168939469", 1723562625}},
	{53014571, "e3d5f4c238ee39cbce543ba777e1665854fad09fee5b6f94caef529c616ac458", rdIn{"1047532039", "1032435578", "18818469941557", "16208608033366", "169558637", 1723562647}},
	{53014577, "ee17d09111b555e4021b42b568d3a9041357ccd7a3936684b59fd90e93c330f6", rdIn{"1047532333", "1032435780", "18818469941557", "16221372958473", "170511703", 1723562681}},
	{53014580, "45bffbfa46eb5646e661b0bd887b34ef90a3301a1e6de98ccd6ebb6f9f247a0a", rdIn{"1047532490", "1032435888", "18818469941557", "16234137881666", "171021054", 1723562699}},
	{53014583, "6adbb68068bfb88c562514dccd3f5980040b3f634131b8288f11599f2cec3bf5", rdIn{"1047532638", "1032435990", "18831421516188", "16234137881666", "171501584", 1723562716}},
}

// Every pair must reconcile exactly (stored b_rate recomputed, backstop_credit delta equal to the
// backstop's share) except the listed ones, which must be reported with their exact residual. The
// window totals are then rebuilt from chain values alone: gross by telescoping the ceil'd
// liabilities over each run of constant d_supply, backstop from the backstop_credit endpoints.
func TestAccrueBetween_Mainnet(t *testing.T) {
	cases := []struct {
		name     string
		v        PoolVersion
		rows     []mainnetVersion
		reported map[uint32]string // ledger → BackstopResidual
	}{
		{"v2_supplies", PoolV2, mainnetV2Supplies, nil},
		{"v2_repays", PoolV2, mainnetV2Repays, nil},
		{"v1_interest_auction", PoolV1, mainnetV1, map[uint32]string{53013801: "-3096224423"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gross, backstop, residual := reconcileMainnet(t, c.v, c.rows, c.reported)
			if want := telescopedGross(t, c.v, c.rows); gross.Cmp(want) != 0 {
				t.Errorf("window gross %s, telescoped from chain values %s", gross, want)
			}
			first, last := c.rows[0].rd.data(t, c.v), c.rows[len(c.rows)-1].rd.data(t, c.v)
			credit := new(big.Int).Sub(last.BackstopCredit, first.BackstopCredit)
			if got := new(big.Int).Add(backstop, residual); got.Cmp(credit) != 0 {
				t.Errorf("window backstop %s + residual %s, chain backstop_credit moved %s", backstop, residual, credit)
			}
		})
	}
}

func reconcileMainnet(t *testing.T, v PoolVersion, rows []mainnetVersion, reported map[uint32]string) (gross, backstop, residual *big.Int) {
	t.Helper()
	gross, backstop, residual = new(big.Int), new(big.Int), new(big.Int)
	for i := 1; i < len(rows); i++ {
		p, n := rows[i-1], rows[i]
		a, err := AccrueBetween(p.rd.data(t, v), n.rd.data(t, v), 2_000_000)
		if err != nil {
			t.Fatalf("ledger %d tx %s: %v", n.ledger, n.tx, err)
		}
		wantResidual := "0"
		if r, ok := reported[n.ledger]; ok {
			wantResidual = r
		}
		if !a.BRateMatches || a.BackstopResidual.String() != wantResidual {
			t.Errorf("ledger %d tx %s: b_rate %s recomputed %s, residual %s want %s",
				n.ledger, n.tx, n.rd.bRate, a.ExpectedBRate, a.BackstopResidual, wantResidual)
		}
		if (p.rd.lt == n.rd.lt) != (a.Gross.Sign() == 0) {
			t.Errorf("ledger %d tx %s: gross %s across last_time %d → %d", n.ledger, n.tx, a.Gross, p.rd.lt, n.rd.lt)
		}
		gross.Add(gross, a.Gross)
		backstop.Add(backstop, a.Backstop)
		residual.Add(residual, a.BackstopResidual)
	}
	return gross, backstop, residual
}

// telescopedGross sums ceil(d_supply·d_rate/S) differences over each run of versions whose d_supply
// (the accrual operand) is constant, using fuzz_test.go's independent rounding helper.
func telescopedGross(t *testing.T, v PoolVersion, rows []mainnetVersion) *big.Int {
	t.Helper()
	s := bi(1_000_000_000_000)
	if v == PoolV1 {
		s = bi(1_000_000_000)
	}
	liab := func(dSupply string, dRate string) *big.Int {
		return refCeil(mul(mustBig(t, dSupply), mustBig(t, dRate)), s)
	}
	total, j := new(big.Int), 0
	for m := 1; m < len(rows); m++ {
		if m == len(rows)-1 || rows[m].rd.dSupply != rows[j].rd.dSupply {
			total.Add(total, liab(rows[j].rd.dSupply, rows[m].rd.dRate))
			total.Sub(total, liab(rows[j].rd.dSupply, rows[j].rd.dRate))
			j = m
		}
	}
	return total
}
