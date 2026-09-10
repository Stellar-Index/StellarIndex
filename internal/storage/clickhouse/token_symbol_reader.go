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
// The value is self-declared by the token contract, so a hostile or
// broken token can put anything in it. SEP-41 and the classic asset code
// space both top out at 12 characters; 32 is generous headroom that
// still refuses a kilobyte of text arriving where a ticker belongs.
const maxTokenSymbolRunes = 32

// TokenSymbol resolves a token contract's `symbol()` from the certified
// lake, by the same route and the same convention as [TokenDecimals]:
// the soroban-token-sdk persists TokenMetadata in the contract INSTANCE
// storage under Symbol "METADATA" as Map{decimal: U32, name: String,
// symbol: String}, so reading the instance entry gives what calling the
// contract would, without executing WASM.
//
// found=false (nil error) when the instance is not captured in the lake,
// the contract stores no METADATA map, the map carries no symbol, or the
// declared value is not a plausible ticker. Callers treat that as "no
// usable metadata" — never as an empty symbol, which would compare equal
// to other things that are also empty.
//
// # What this value may and may not be used for
//
// It is CONTRACT-AUTHORED text, with all that implies. It is exactly as
// trustworthy as a classic asset code, which is to say not at all on its
// own: any contract may call itself BENJI, and the lake holds many that
// call themselves things they are not.
//
// The RWA definition reads it only under requirement C4, and only AFTER
// C2 and C3 have established that an independent third party named that
// exact contract address. In that position it answers WHICH instrument
// an already-vouched-for address holds. It must never be used to answer
// WHETHER an address is vouched for — a join on this value alone is the
// code-only identity every part of that definition refuses.
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
// The token-sdk writes symbol as an ScString; some tokens write an
// ScSymbol instead, which is the same text in a different ScVal variant.
// Both are accepted — refusing the second would report "no metadata" for
// a token that plainly has some — and nothing else is.
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
// The refusal is deliberate rather than a best-effort clean-up. This
// value is compared against an allow-list of instrument codes, and a
// declaration carrying control characters, newlines or an essay is not a
// ticker that happens to need tidying — it is a token doing something
// other than declaring a symbol. Reporting it as absent is both true and
// safe; silently trimming it toward a match is neither.
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
