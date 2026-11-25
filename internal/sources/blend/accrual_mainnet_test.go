package blend

import (
	"math/big"
	"testing"
)

// Consecutive stored ResData versions of one reserve (asset contract ADEFCE59…E496) of mainnet Blend V2
// pool CAJJZSGMMM3PD7N33TAPHGBUGTB43OC73HVIK2L2G6BNGGGYOSSYBXBD, read from stellar.ledger_entry_changes.
// Every version follows a supply or withdraw in the same tx (b_supply moves); the two at ledger
// 65641374 share a timestamp. The pool's Config bstop_rate is 0_2000000.
var mainnetReserveVersions = []struct {
	ledger uint32
	tx     string
	rd     rdIn // d_rate, b_rate, b_supply, d_supply, backstop_credit, last_time
}{
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

// Every consecutive pair must reconcile exactly: the stored b_rate equals the recomputed one and the
// backstop_credit delta equals the backstop's floor share, with no residual.
func TestAccrueBetween_MainnetReserveVersions(t *testing.T) {
	const bstopRate = 2_000_000
	sumGross, sumBackstop := new(big.Int), new(big.Int)
	for i := 1; i < len(mainnetReserveVersions); i++ {
		p, n := mainnetReserveVersions[i-1], mainnetReserveVersions[i]
		a, err := AccrueBetween(p.rd.data(t, PoolV2), n.rd.data(t, PoolV2), bstopRate)
		if err != nil {
			t.Fatalf("ledger %d tx %s: %v", n.ledger, n.tx, err)
		}
		if !a.BRateMatches || a.BackstopResidual.Sign() != 0 {
			t.Errorf("ledger %d tx %s: b_rate want %s got %s, backstop residual %s",
				n.ledger, n.tx, n.rd.bRate, a.ExpectedBRate, a.BackstopResidual)
		}
		if (p.rd.lt == n.rd.lt) != (a.Gross.Sign() == 0) {
			t.Errorf("ledger %d tx %s: gross %s across last_time %d → %d", n.ledger, n.tx, a.Gross, p.rd.lt, n.rd.lt)
		}
		sumGross.Add(sumGross, a.Gross)
		sumBackstop.Add(sumBackstop, a.Backstop)
	}
	// 266387931 is the stored backstop_credit's rise over the window (1860490069203 − 1860223681272).
	if sumGross.String() != "1331939674" || sumBackstop.String() != "266387931" {
		t.Errorf("window gross %s backstop %s, want 1331939674 and 266387931", sumGross, sumBackstop)
	}
}
