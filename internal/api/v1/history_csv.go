package v1

import (
	"net/http"
	"strconv"

	explorerpkg "github.com/Stellar-Index/StellarIndex/internal/api/v1/explorer"
)

var historyCSVColumns = []string{
	"source", "ledger", "tx_hash", "op_index", "ts", "base_asset", "quote_asset",
	"base_amount", "quote_amount", "price", "base_decimals", "quote_decimals", "routed_via",
}

// writeHistoryCSV writes one /v1/history page with the JSON's exact cell
// text: amounts stay integer strings and price the decimal string.
func (s *Server) writeHistoryCSV(w http.ResponseWriter, r *http.Request, rows []TradeRow, env Envelope) {
	p := explorerpkg.CSVPage{Columns: historyCSVColumns, Rows: make([][]string, len(rows))}
	if env.Pagination != nil {
		p.NextCursor = env.Pagination.Next
	}
	if env.Flags.OutsideCoverage {
		p.Flags = append(p.Flags, "outside_coverage")
	}
	if env.CoverageFrom != nil {
		p.Headers = map[string]string{"X-StellarIndex-Coverage-From": env.CoverageFrom.String()}
	}
	for i, t := range rows {
		price, baseDec, quoteDec := "", "", ""
		if t.Price != nil {
			price = *t.Price
		}
		if t.BaseDecimals != 0 {
			baseDec = strconv.Itoa(t.BaseDecimals)
		}
		if t.QuoteDecimals != 0 {
			quoteDec = strconv.Itoa(t.QuoteDecimals)
		}
		p.Rows[i] = []string{
			t.Source, strconv.FormatUint(uint64(t.Ledger), 10), t.TxHash, strconv.FormatUint(uint64(t.OpIndex), 10),
			t.Timestamp.String(), t.BaseAsset, t.QuoteAsset, t.BaseAmount, t.QuoteAmount, price,
			baseDec, quoteDec, t.RoutedVia,
		}
	}
	if err := explorerpkg.WriteCSVPage(w, r, p); err != nil && !clientAborted(r, err) {
		s.logger.Warn("history CSV write failed", "err", err)
	}
}
