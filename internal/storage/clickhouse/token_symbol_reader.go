package clickhouse

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// maxTokenSymbolRunes bounds the accepted on-chain `symbol` declaration.
// The value is self-declared by the token contract, so a hostile or broken token can put
// anything in it. SEP-41 and classic asset codes top out at 12 characters; 32 leaves headroom
// while refusing a kilobyte of text where a ticker belongs.
const maxTokenSymbolRunes = 32

// TokenSymbol resolves a token contract's symbol() from its instance METADATA map; the text is contract-authored and untrusted (any contract may call itself BENJI), so never use it alone as an identity; found=false means no usable metadata.
func (r *ExplorerReader) TokenSymbol(ctx context.Context, contractID string) (string, bool, error) {
	raw, err := strkey.Decode(strkey.VersionByteContract, contractID)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: TokenSymbol: bad contract id %q: %w", contractID, err)
	}
	var cid xdr.Hash
	copy(cid[:], raw)
	keys, err := instanceKeyXDR(cid)
	if err != nil {
		return "", false, err
	}
	// Same table + shape as TokenDecimals: ledger_entries_current is
	// merge-loss immune and (entry_type, key_xdr) is a PK-prefix lookup.
	const q = `SELECT entry_xdr FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'contract_data' AND key_xdr IN (?) AND entry_xdr != ''
		ORDER BY ledger_seq DESC LIMIT 1`
	rows, err := r.conn.Query(ctx, q, keys)
	if err != nil {
		return "", false, fmt.Errorf("clickhouse: token symbol scan: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return "", false, rows.Err()
	}
	var b64 string
	if err := rows.Scan(&b64); err != nil {
		return "", false, fmt.Errorf("clickhouse: scan token symbol: %w", err)
	}
	sym, ok := symbolFromInstanceEntry(b64)
	return sym, ok, rows.Err()
}

// symbolFromInstanceEntry decodes one contract-instance LedgerEntry and
// returns the token-sdk METADATA map's `symbol` string.
//
// The token-sdk writes symbol as an ScString; some tokens write an ScSymbol, the same text in
// another ScVal variant. Both are accepted, since refusing the second would report "no
// metadata" for a token that has some; nothing else is.
func symbolFromInstanceEntry(b64 string) (string, bool) {
	var entry xdr.LedgerEntry
	if xdr.SafeUnmarshalBase64(b64, &entry) != nil {
		return "", false
	}
	cd, ok := entry.Data.GetContractData()
	if !ok {
		return "", false
	}
	inst, ok := cd.Val.GetInstance()
	if !ok || inst.Storage == nil {
		return "", false
	}
	for _, kv := range *inst.Storage {
		sym, ok := kv.Key.GetSym()
		if !ok || string(sym) != "METADATA" || kv.Val.Type != xdr.ScValTypeScvMap || kv.Val.Map == nil {
			continue
		}
		for _, e := range **kv.Val.Map {
			ksym, ok := e.Key.GetSym()
			if !ok || string(ksym) != "symbol" {
				continue
			}
			return sanitiseTokenSymbol(scValText(e.Val))
		}
	}
	return "", false
}

// scValText extracts the text of an ScString or ScSymbol, or "" for
// anything else.
func scValText(v xdr.ScVal) string {
	if s, ok := v.GetStr(); ok {
		return string(s)
	}
	if s, ok := v.GetSym(); ok {
		return string(s)
	}
	return ""
}

// sanitiseTokenSymbol trims a declared symbol and refuses one that is
// not a plausible ticker.
//
// The refusal is deliberate: the value is compared against an allow-list of instrument codes, and
// control characters, newlines or an essay mean the token is not declaring a symbol at all.
// Reporting it absent is true and safe; trimming it toward a match is neither.
func sanitiseTokenSymbol(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	if len([]rune(s)) > maxTokenSymbolRunes {
		return "", false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return "", false
		}
	}
	return s, true
}
