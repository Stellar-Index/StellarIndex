// Package sep41_supply is the SEP-41 supply-event observer (ADR-0023): a
// [dispatcher.Decoder] emitting one Event per mint, burn or clawback on a
// contract in `[supply] watched_sep41_contracts`. Transfers do not change
// supply (Algorithm 3: Σ mint − Σ(burn + clawback)) and are filtered at Match.
//
// Topic count does not identify the shape; the counterparty position does:
//
//	legacy SAC   ["mint"|"clawback", admin, to|from]        counterparty @ topic[2]
//	CAP-67       ["mint"|"burn"|"clawback", to|from, asset] counterparty @ topic[1]
//	bare SEP-41  ["mint"|"burn", to|from]                   counterparty @ topic[1]
//
// [decodeCounterparty] takes topic[2] iff it is an Address (sep0011_asset
// is an ScvString), else topic[1]. The body is a bare i128 or a CAP-67 map
// {amount, to_muxed_id} (muxed destinations, issuer memo strings);
// [decodeAmount] type-tests both, since an i128-only decode drops every map
// row. Amounts are wire stroops, stored verbatim.
package sep41_supply
