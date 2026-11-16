package blnt

// The BLNT V3 event ABI as data: one spec per (role, topic[0]). Each spec names every topic and body
// field in wire order with its type and the blnt_events column it lands in. Transcribed from the
// contracts' events.rs at v3.0.2 and checked against a real event of every kind the deployment has
// emitted (see the fixtures and docs/operations/wasm-audits/blnt.md).

type fieldType uint8

const (
	tAddr    fieldType = iota // Address
	tOptAddr                  // Option<Address>
	tI128                     // i128 amount
	tU32
	tU64
	tTier // BackstopTier: a unit-variant enum, on the wire Vec[Symbol]
	tAny  // struct, map or list: rendered losslessly into attributes
)

type column uint8

const (
	cAttr column = iota // attributes[name]
	cPool
	cUser
	cAsset
	cTier
	cAmount
	cAmount2
)

type field struct {
	name     string
	typ      fieldType
	col      column
	optional bool // a trailing topic some emit sites omit
}

type bodyShape uint8

const (
	bodyUnit   bodyShape = iota // no payload: Void on the wire
	bodySingle                  // the body IS the one data field
	bodyVec                     // the body is a Vec of the data fields
)

type spec struct {
	topics []field // after topic[0]
	body   bodyShape
	data   []field
}

func attr(name string, t fieldType) field { return field{name: name, typ: t} }

func amount(name string) field  { return field{name: name, typ: tI128, col: cAmount} }
func amount2(name string) field { return field{name: name, typ: tI128, col: cAmount2} }

var (
	fAsset = field{name: "asset", typ: tAddr, col: cAsset}
	fTier  = field{name: "tier", typ: tTier, col: cTier}
	fAdmin = attr("admin", tAddr)
)

func user(name string) field { return field{name: name, typ: tAddr, col: cUser} }
func pool(name string) field { return field{name: name, typ: tAddr, col: cPool} }

func single(f field, topics ...field) spec {
	return spec{topics: topics, body: bodySingle, data: []field{f}}
}

func vec(topics []field, data ...field) spec {
	return spec{topics: topics, body: bodyVec, data: data}
}

func topics(f ...field) []field { return f }

// position is the shared shape of the six money-market moves: tokens, then the b/d-token delta.
func position(tokens, shares string) spec {
	return vec(topics(fAsset, user("from")), amount(tokens), amount2(shares))
}

var auctionTopics = topics(attr("auction_type", tU32), user("user"))

var abi = map[Role]map[string]spec{
	RolePool: {
		"set_admin":          single(attr("new_admin", tAddr), fAdmin),
		"update_pool":        vec(topics(fAdmin), attr("backstop_take_rate", tU32), attr("max_positions", tU32), attr("min_collateral", tI128)),
		"queue_set_reserve":  vec(topics(fAdmin), fAsset, attr("metadata", tAny)),
		"cancel_set_reserve": single(fAsset, fAdmin),
		"set_reserve":        vec(nil, fAsset, attr("index", tU32)),
		// Two emit sites: the permissionless status update has no admin topic. The source calls the body
		// new_status at one and pool_status at the other; both land as `status`.
		"set_status":              single(attr("status", tU32), field{name: "admin", typ: tAddr, optional: true}),
		"reserve_emission_update": vec(nil, attr("res_token_id", tU32), attr("eps", tU64), attr("expiration", tU64)),
		"gulp_emissions":          single(amount("emissions")),
		"claim":                   vec(topics(user("from")), attr("reserve_token_ids", tAny), amount("amount_claimed")),
		"bad_debt":                single(amount("d_tokens"), user("user"), fAsset),
		"defaulted_debt":          single(amount("d_tokens_burnt"), fAsset),
		"supply":                  position("tokens_in", "b_tokens_minted"),
		"withdraw":                position("tokens_out", "b_tokens_burnt"),
		"supply_collateral":       position("tokens_in", "b_tokens_minted"),
		"withdraw_collateral":     position("tokens_out", "b_tokens_burnt"),
		"borrow":                  position("tokens_out", "d_tokens_minted"),
		"repay":                   position("tokens_in", "d_tokens_burnt"),
		"flash_loan":              vec(topics(fAsset, user("from"), attr("contract", tAddr)), amount("tokens_out"), amount2("d_tokens_minted")),
		"clawback":                vec(topics(fAsset, user("from")), amount("amount"), attr("supply_burned", tI128), attr("collateral_burned", tI128)),
		"gulp":                    single(amount("token_delta"), fAsset),
		"reconcile_loss":          vec(topics(fAsset), amount("loss"), attr("supplier_loss", tI128), attr("backstop_credit_loss", tI128), attr("b_rate_loss", tI128)),
		"protocol_credit_loss":    single(amount("amount"), fAsset),
		"new_auction":             vec(auctionTopics, attr("percent", tU32), attr("auction_data", tAny)),
		"fill_auction":            vec(auctionTopics, attr("filler", tAddr), attr("fill_percent", tI128), attr("filled_auction_data", tAny)),
		"delete_auction":          {topics: auctionTopics, body: bodyUnit},
	},
	RoleFactory: {
		"deploy": single(pool("pool_address")),
	},
	// Backstop deposits and withdrawals keep one convention whatever the wire order: amount is backstop
	// tokens, amount2 is shares. The contract's `amount` on the three withdrawal events is a share count.
	RoleBackstop: {
		"migration_activated": vec(nil, attr("activated_at", tU64), attr("backfill_end", tU64)),
		"backfill_funded":     single(amount("amount")),
		"deposit":             vec(topics(fTier, pool("pool_address"), user("from")), amount("tokens_in"), amount2("shares_minted")),
		"queue_withdrawal":    vec(topics(fTier, pool("pool_address"), user("from")), amount2("amount"), attr("expiration", tU64)),
		"dequeue_withdrawal":  single(amount2("amount"), fTier, pool("pool_address"), user("from")),
		"withdraw":            vec(topics(fTier, pool("pool_address"), user("from")), amount2("amount"), amount("tokens_out")),
		"distribute":          single(amount("new_tokens_emitted")),
		"gulp_emissions":      vec(topics(pool("pool_address")), amount("new_backstop_emissions"), amount2("new_pool_emissions")),
		"rw_zone_add":         vec(nil, pool("to_add"), attr("to_remove", tOptAddr)),
		"rw_zone_remove":      single(pool("to_remove")),
		"claim":               vec(topics(fTier, user("user"), pool("pool")), amount("blnt_amount"), amount2("lp_amount"), attr("shares", tI128)),
		"draw":                vec(topics(fTier, pool("pool_address")), attr("to", tAddr), amount("amount")),
		"donate":              single(amount("amount"), fTier, pool("pool_address"), user("from")),
	},
	RoleEmitter: {
		"distribute": vec(nil, attr("backstop", tAddr), amount("amount")),
		"q_swap":     single(attr("swap", tAny)),
		"del_swap":   single(attr("swap", tAny)),
		"swap":       single(attr("swap", tAny)),
		"drop":       single(attr("list", tAny)),
	},
	RoleBackfill: {
		"claim":        single(amount("amount"), user("user")),
		"claim_grant":  single(amount("amount"), user("user")),
		"swap_blnd":    vec(topics(user("user")), amount("blnd_burned"), amount2("blnt_received"), attr("total_blnt_swapped", tI128)),
		"burn_expired": vec(nil, amount("blnt"), attr("total_blnt_burned", tI128)),
	},
	RoleLockupBackfill: {
		"fund":       vec(topics(user("from")), amount("amount"), attr("balance", tI128)),
		"claim":      vec(topics(user("beneficiary")), amount("amount"), attr("total_claimed", tI128)),
		"claim_fund": single(attr("details", tAny)),
		"expire":     vec(topics(user("owner")), amount("amount"), attr("total_claimed", tI128)),
	},
}
