// lint-pk-discriminators enforces ADR-0033's anti-data-loss rule: every
// protocol-row hypertable — a table that stores one row per emitted
// contract event / trade — MUST key on a PER-EVENT discriminator, so that
// when a single operation emits several rows of the same kind (a path-payment
// crossing multiple offers, a Blend action touching several positions, a
// multi-feed oracle push) they don't collide on the primary key and get
// silently dropped by ON CONFLICT DO NOTHING.
//
// The canonical discriminator is `event_index` (the contract event's index
// within its transaction). A table may instead use an equally-unique
// alternative (e.g. (asset, user_address) for blend_positions) — those are
// recorded in the allow map below WITH A REASON. A protocol-row table that
// has neither fails the lint.
//
// This is the static, PR-time complement to the data-driven guard
// (compute-completeness's aggregate reconcile, run on a timer): the reconcile
// catches a collision in production as a projection delta; this catches a new
// table that would introduce one before it ships.
//
// Usage: go run ./scripts/ci/lint-pk-discriminators
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// protocolRowTables are the hypertables that store one row per emitted
// event/trade. Adding a new such table here is the point: it forces a
// discriminator decision. Pure observation/registry/aggregate tables are not
// listed (they key on the observed entity, not per-event).
var protocolRowTables = []string{
	"trades",
	"oracle_updates",
	"blend_positions", "blend_emissions", "blend_admin", "blend_auctions",
	"phoenix_liquidity", "phoenix_stake_events",
	"comet_liquidity",
	"soroswap_skim_events", "soroswap_router_swaps",
	"cctp_events", "rozo_events",
	"defindex_flows", "defindex_fees", "defindex_admin_events",
	"sep41_supply_events", "sep41_transfers",
	"credit_positions", "credit_statements", "credit_settlements", "credit_events",
}

// allow maps a protocol-row table to the reason its PK is collision-free
// WITHOUT event_index. Two flavours:
//   - "OK:"   a genuine alternative discriminator — permanent.
//   - "TODO:" a known coarse-PK data-loss bug being fixed (remove the entry
//     when the fix lands; the lint then enforces event_index).
var allow = map[string]string{
	"oracle_updates":        "OK: decoders fan out op_index per feed; (source,ledger,tx,op,ts) is unique per update",
	"cctp_events":           "OK: (contract_id,…,event_type,ts) — one event per (type,ts) per op",
	"rozo_events":           "OK: (contract_id,…,event_type,ts) — one event per (type,ts) per op",
	"trades":                "OK: op_index is FANNED (canonical.FanoutOpIndex: opIndex<<16|event_index; SDEX opIdx*1024+claim_index) — encodes the per-trade discriminator, so multi-trade ops never collide",
	"soroswap_router_swaps": "OK: call_sig (RouterSwap.CallSig content hash; migration 0056) discriminates distinct swaps in one op; auth-tree dups share it + dedup; completeness reconcile Δ=0",
	// blend_auctions (0058), comet_liquidity (0059), phoenix_liquidity +
	// phoenix_stake_events (0060), sep41_supply_events (0057) were removed
	// from this allow map when F-1324 added event_index to their PKs — the
	// lint now ENFORCES event_index on them (their previous (kind,token) /
	// (action) / observed_at keys did NOT discriminate two same-op events).
}

var (
	stmtSplit    = regexp.MustCompile(`;`)
	ws           = regexp.MustCompile(`\s+`)
	lineComment  = regexp.MustCompile(`--[^\n]*`)
	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	createRe     = regexp.MustCompile(`(?i)CREATE TABLE\s+(?:IF NOT EXISTS\s+)?([a-z0-9_]+)`)
	alterRe      = regexp.MustCompile(`(?i)ALTER TABLE\s+(?:ONLY\s+)?([a-z0-9_]+)`)
	pkRe         = regexp.MustCompile(`(?i)PRIMARY KEY\s*\(([^)]+)\)`)
	// An ALTER adding event_index with a DEFAULT: every existing row gets
	// that constant instead of its true index.
	backfillRe = regexp.MustCompile(`(?i)^ALTER TABLE\s+(?:ONLY\s+)?([a-z0-9_]+)\b.*\bADD COLUMN\s+(?:IF NOT EXISTS\s+)?event_index\b[^,]*\bDEFAULT\b`)
	// A whole-table wipe; a DELETE with a WHERE clause does not match.
	wipeRe = regexp.MustCompile(`(?i)^(?:DELETE\s+FROM|TRUNCATE(?:\s+TABLE)?)\s+(?:ONLY\s+)?([a-z0-9_]+)$`)
)

// legacyBackfill maps a protocol-row table whose event_index was added by
// ALTER … ADD COLUMN … DEFAULT over already-populated rows, and never
// disarmed by a later whole-table DELETE/TRUNCATE, to why that is still
// tolerated. The writer stamps each event's TRUE in-op index, so a replay
// writes the same event under a new PK beside its legacy default-index row
// (0137 comet, 0164 cctp/rozo). A new backfill must ship its own disarm;
// only a "TODO:" entry naming the pending disarm may wave it through.
var legacyBackfill = map[string]string{
	"blend_emissions":      "TODO: 0053 left legacy event_index=0 rows; its DELETE + re-derive is an operator-runbook step no migration enforces",
	"blend_admin":          "TODO: 0053 left legacy event_index=0 rows; its DELETE + re-derive is an operator-runbook step no migration enforces",
	"blend_positions":      "TODO: 0054 left legacy event_index=0 rows; its DELETE + re-derive is an operator-runbook step no migration enforces",
	"defindex_flows":       "TODO: 0055 left legacy event_index=0 rows; its DELETE + re-derive is an operator-runbook step no migration enforces",
	"sep41_supply_events":  "TODO: 0057 left legacy event_index=0 rows; its DELETE + re-derive is an operator-runbook step no migration enforces",
	"blend_auctions":       "TODO: 0058 left legacy event_index=0 rows; its own header makes the DELETE + re-derive optional",
	"phoenix_liquidity":    "TODO: 0060 left legacy event_index=0 rows and a projector-replay doubles them (CA2-A34); whole-table disarm migration pending",
	"phoenix_stake_events": "TODO: 0060 left legacy event_index=0 rows and a projector-replay doubles them (CA2-A34); whole-table disarm migration pending",
}

// statements returns f's SQL statements with comments stripped and
// whitespace collapsed.
func statements(f string) ([]string, error) {
	b, err := os.ReadFile(f) //nolint:gosec // f comes from a fixed Glob of migrations/, not user input
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", f, err)
	}
	// Strip comments first — a `;` inside a comment would otherwise split a
	// CREATE statement away from its PRIMARY KEY clause.
	content := blockComment.ReplaceAllString(string(b), "")
	content = lineComment.ReplaceAllString(content, "")
	parts := stmtSplit.Split(content, -1)
	out := make([]string, 0, len(parts))
	for _, stmt := range parts {
		out = append(out, strings.TrimSpace(ws.ReplaceAllString(stmt, " ")))
	}
	return out, nil
}

// legacyBackfillViolations fails every table whose latest event_index DEFAULT
// backfill is not followed, in the same or a later migration, by a whole-table
// DELETE/TRUNCATE, unless allowed carries a TODO for it. Entries that no
// longer apply come back as non-fatal drift.
func legacyBackfillViolations(entries, tables []string, allowed map[string]string) (failures, stale []string, err error) {
	lastBackfill, lastWipe := map[string]int{}, map[string]int{}
	for i, f := range entries {
		stmts, serr := statements(f)
		if serr != nil {
			return nil, nil, serr
		}
		for _, norm := range stmts {
			if m := backfillRe.FindStringSubmatch(norm); m != nil {
				lastBackfill[strings.ToLower(m[1])] = i + 1
			}
			if m := wipeRe.FindStringSubmatch(norm); m != nil {
				lastWipe[strings.ToLower(m[1])] = i + 1
			}
		}
	}
	for _, t := range tables {
		b := lastBackfill[t]
		armed := b > 0 && lastWipe[t] < b
		reason, listed := allowed[t]
		switch {
		case armed && listed && strings.HasPrefix(reason, "TODO"):
			// known and tracked
		case armed:
			failures = append(failures, fmt.Sprintf("%-22s %s adds event_index with a DEFAULT over existing rows and no later migration wipes the table — a replay re-writes every legacy event under its true event_index; ship a whole-table DELETE (decompress first) and the projector-replay follow-up", t, filepath.Base(entries[b-1])))
		case listed:
			stale = append(stale, fmt.Sprintf("%-22s has no un-disarmed event_index backfill — remove its legacyBackfill entry", t))
		}
	}
	return failures, stale, nil
}

// parsePKs returns the latest PRIMARY KEY column list per table across the
// given migration files (sorted ascending so later definitions win).
func parsePKs(entries []string) (map[string]string, error) {
	pk := map[string]string{}
	for _, f := range entries {
		stmts, serr := statements(f)
		if serr != nil {
			return nil, serr
		}
		for _, norm := range stmts {
			m := pkRe.FindStringSubmatch(norm)
			if m == nil {
				continue
			}
			var table string
			if cm := createRe.FindStringSubmatch(norm); cm != nil {
				table = cm[1]
			} else if am := alterRe.FindStringSubmatch(norm); am != nil {
				table = am[1]
			} else {
				continue
			}
			pk[strings.ToLower(table)] = strings.ToLower(strings.TrimSpace(m[1]))
		}
	}
	return pk, nil
}

func main() {
	root := "migrations"
	entries, err := filepath.Glob(filepath.Join(root, "*.up.sql"))
	if err != nil || len(entries) == 0 {
		fmt.Fprintf(os.Stderr, "lint-pk-discriminators: no migrations found under %s/\n", root)
		os.Exit(2)
	}
	sort.Strings(entries) // 0001 < 0002 < … so later PK definitions win

	pk, perr := parsePKs(entries)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "lint-pk-discriminators: %v\n", perr)
		os.Exit(2)
	}

	var failures, stale []string
	for _, t := range protocolRowTables {
		cols, ok := pk[t]
		if !ok {
			failures = append(failures, fmt.Sprintf("%-22s no PRIMARY KEY found in migrations (typo in the table list?)", t))
			continue
		}
		hasEventIndex := strings.Contains(cols, "event_index")
		reason, allowed := allow[t]
		switch {
		case hasEventIndex && allowed && strings.HasPrefix(reason, "TODO"):
			// Fixed but still listed — nudge to tidy the allowlist.
			stale = append(stale, fmt.Sprintf("%-22s now has event_index — remove its TODO allowlist entry", t))
		case hasEventIndex:
			// good
		case allowed:
			// justified alternative or tracked TODO — fine for now
		default:
			failures = append(failures, fmt.Sprintf("%-22s PK (%s) lacks event_index and has no allowlist entry — add a per-event discriminator or document why it's collision-free", t, cols))
		}
	}

	bfFail, bfStale, berr := legacyBackfillViolations(entries, protocolRowTables, legacyBackfill)
	if berr != nil {
		fmt.Fprintf(os.Stderr, "lint-pk-discriminators: %v\n", berr)
		os.Exit(2)
	}
	failures = append(failures, bfFail...)
	stale = append(stale, bfStale...)

	if len(stale) > 0 {
		fmt.Println("lint-pk-discriminators: allowlist drift (non-fatal):")
		for _, s := range stale {
			fmt.Println("  •", s)
		}
	}
	if len(failures) > 0 {
		fmt.Fprintln(os.Stderr, "lint-pk-discriminators: FAIL — protocol-row tables without a per-event PK discriminator, or with an un-disarmed event_index backfill:")
		for _, f := range failures {
			fmt.Fprintln(os.Stderr, "  ✗", f)
		}
		fmt.Fprintln(os.Stderr, "\nSee ADR-0033 / the coarse-PK data-loss class. Either add event_index to the PK")
		fmt.Fprintln(os.Stderr, "or, if the PK is genuinely collision-free, add the table to `allow` with an OK: reason.")
		fmt.Fprintln(os.Stderr, "A backfill finding is cleared by a whole-table DELETE migration (0137 / 0164), not by `allow`.")
		os.Exit(1)
	}
	fmt.Printf("lint-pk-discriminators: OK — %d protocol-row tables checked; all key on a per-event discriminator (or justified)\n", len(protocolRowTables))
}
