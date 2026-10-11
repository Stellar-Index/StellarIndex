// Package supply derives total, circulating and max supply per asset class (ADR-0011): [XLMComputer]
// (fixed supply, SDF-reserve exclusion), [ClassicComputer] (ADR-0022) and [SEP41Computer] (ADR-0023).
// Amounts are *big.Int end to end (ADR-0003); JSON encoding happens at the API boundary.
package supply
