package v1

// declaredPegMayNotReplace reports a withholding the declared-peg fill must leave standing: an operator excluded a source behind
// this asset's price, typically because its market is being manipulated or has depegged, which a fixed 1:1 value would hide.
func declaredPegMayNotReplace(row *AssetDetail) bool {
	return row.PriceUSD == nil && row.PriceWithheldReason == PriceWithheldSourceExcluded
}
