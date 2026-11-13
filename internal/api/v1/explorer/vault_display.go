package explorer

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// vaultDisplay is what a DeFindex vault's own share-token metadata says about it: a vault IS the SEP-41 contract of its dfTokens.
type vaultDisplay struct {
	Label    string
	Decimals *int // nil when unread: the amount stays in base units, never scaled by a guessed 7
}

// vaultNameMaxRunes bounds the contract-authored name; any contract may name itself anything.
const vaultNameMaxRunes = 48

// vaultDisplays batch-reads each vault's share-token metadata in one lake query; a failed read yields an empty map.
func (h *Handler) vaultDisplays(ctx context.Context, vaults []string) map[string]vaultDisplay {
	out := map[string]vaultDisplay{}
	if h.Reader == nil || len(vaults) == 0 {
		return out
	}
	metas, err := h.Reader.TokenDisplays(ctx, vaults)
	if err != nil {
		h.Logger.Warn("positions: vault metadata read failed; share amounts served unscaled", "err", err)
		return out
	}
	for v, m := range metas {
		if !m.HasMeta {
			continue
		}
		d := int(m.Decimals)
		out[v] = vaultDisplay{Label: vaultLabel(m.Name, m.Symbol), Decimals: &d}
	}
	return out
}

// vaultLabel renders "Greep Vault (GRPVLT)" from the factory's "DeFindex-Vault-Greep Vault" name; an implausible name or symbol is dropped.
func vaultLabel(name, symbol string) string {
	name = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(name), "DeFindex-Vault-"))
	if !plausibleDisplayText(name, vaultNameMaxRunes) {
		name = ""
	}
	symbol = strings.TrimSpace(symbol)
	if !plausibleDisplayText(symbol, 12) || strings.ContainsFunc(symbol, unicode.IsSpace) {
		symbol = ""
	}
	switch {
	case name != "" && symbol != "":
		return fmt.Sprintf("%s (%s)", name, symbol)
	case name != "":
		return name
	case symbol != "":
		return symbol + " vault"
	}
	return ""
}

func plausibleDisplayText(s string, maxRunes int) bool {
	if s == "" || len([]rune(s)) > maxRunes {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}
