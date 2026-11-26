package blendtest

// Default is one mainnet Blend V2 reserve version written by a transaction that emitted defaulted_debt for its
// asset: Prev is the transaction's state pre-image, Next its updated version, both decoded from
// internal/sources/blend/testdata/defaulted_debt/reserves.tsv (TestDefaulted_GoldenMatchesXDR pins them).
type Default struct {
	Pool, Asset string
	BstopRate   uint32 // the one rate at which the transaction's backstop_credit delta is exactly accrual's share
	Ledger      uint32
	Tx          string
	EventIndex  uint32 // the defaulted_debt event's index; its op index is 0
	DTokens     string // that event's d_tokens_burnt
	Prev, Next  Row    // Prev.Ledger is the pre-image's last-modified ledger; Prev.Tx is unknown
	// SupplyResidual is what the production lending_interest_hourly row stored for the version.
	SupplyResidual string
}

const (
	defaultPool1 = "CCCCIQSDILITHMM7PBSLVDT5MISSY7R26MNZXCX4H7J5JQ5FPIYOGYFS"
	defaultPool2 = "CBYOBT7ZCCLQCBUYYIABZLSEGDPEUWXCUXQTZYOG3YBDR7U357D5ZIRF"
	defaultTx1   = "097d5ce03412177cf393c44b8dc0024cd0698fa815f7a6c53423b7cccd1a69e9"
	defaultTx2   = "a665da35591d8e1ea8a7273b71a0a126511cecd330bff54298ba47483a4f1e09"
	defaultTx3   = "05709a139c88cae4acc6b05a6337b60567f43a4923daf49d7bf3564920dc7cf7"
	cbco65       = "CBCO65UOWXY2GR66GOCMCN6IU3Y45TXCPBY3FLUNL4AOUMOCKVIVV6JC"
	cbn3nc       = "CBN3NCJSMOQTC6SPEYK3A44NU4VS3IPKTARJLI3Y77OH27EWBY36TP7U"
	cdikur       = "CDIKURWHYS4FFTR5KOQK6MBFZA2K3E26WGBQI6PXBYWZ4XIOPJHDFJKP"
)

// DefaultedDebt is every version the full-history interest proof left unexplained before revision 3, in reserves.tsv's
// order. In 61341315 the CCW67TSZ reserve's b_supply also moved; in 64534597 the CDIKURWH cut exceeds b_rate.
var DefaultedDebt = []Default{
	{
		defaultPool1, cas3j, 2_000_000, 61341315, defaultTx1, 2, "590795068405660",
		Row{61341303, "", "1002697396828", "1000430612515", "1139120447680522", "657227055216993", "855058583", 1771725066, "", ""},
		Row{61341315, defaultTx1, "1002697420123", "480390190213", "1139120447680522", "66431986811333", "858120603", 1771725137, "", ""},
		"-592388690912445",
	},
	{
		defaultPool1, usdc, 2_000_000, 61341315, defaultTx1, 3, "8339672984961",
		Row{61341314, "", "1176407668240", "1115908765680", "62892166765053", "59657637589837", "3066671603", 1771725131, "", ""},
		Row{61341315, defaultTx1, "1176408900963", "959914669718", "62895729106146", "51317964604876", "3081379871", 1771725137, "", ""},
		"-9810865530649",
	},
	{
		defaultPool1, cas3j, 2_000_000, 61342113, defaultTx2, 2, "3796680359969",
		Row{61342091, "", "1002697666847", "480390198498", "758039810916782", "32448486063345", "153037676", 1771729695, "", ""},
		Row{61342113, defaultTx2, "1002697674100", "475368137224", "758039810916782", "28611881291095", "153084745", 1771729828, "", ""},
		"-3806922566550",
	},
	{
		defaultPool1, usdc, 2_000_000, 61342113, defaultTx2, 3, "12485492914",
		Row{61342111, "", "1176879576227", "960216650978", "49122763106078", "8084493312024", "2227954054", 1771729817, "", ""},
		Row{61342113, defaultTx2, "1176879620211", "959917530229", "49122763106078", "8071802428250", "2228025171", 1771729828, "", ""},
		"-14693922163",
	},
	{
		defaultPool1, cdtkp, 2_000_000, 61342113, defaultTx2, 4, "120464150556",
		Row{61342067, "", "1105988541203", "1063802699836", "3494625746180", "2422521187900", "34839406", 1771729557, "", ""},
		Row{61342113, defaultTx2, "1105989379834", "1025678318124", "3494625746180", "2302057037344", "35245726", 1771729828, "", ""},
		"-133232071169",
	},
	{
		defaultPool2, cbco65, 5_000_000, 64534597, defaultTx3, 2, "11442141",
		Row{64143339, "", "1107144041443", "1050101064550", "416729128", "17403739", "553914894", 1780106584, "", ""},
		Row{64534597, defaultTx3, "1107547064869", "1019699534897", "416729128", "5961598", "553918401", 1789958763, "", ""},
		"-12672710",
	},
	{
		defaultPool2, cbn3nc, 5_000_000, 64534597, defaultTx3, 1, "63676479",
		Row{63605734, "", "1052615993777", "1023372658670", "313810229", "63699923", "115382876", 1784782622, "", ""},
		Row{64534597, defaultTx3, "1052924043484", "809750927525", "313810229", "23444", "115392687", 1789958763, "", ""},
		"-67046496",
	},
	{
		defaultPool2, cdikur, 5_000_000, 64534597, defaultTx3, 0, "44341258274",
		Row{64143339, "", "1159777255752", "1074868422420", "52362783699", "47841211574", "3932576218", 1779822683, "", ""},
		Row{64534597, defaultTx3, "2855088014418", "0", "52362783699", "3499953300", "44485436562", 1789958763, "", ""},
		"-96835963052",
	},
}
