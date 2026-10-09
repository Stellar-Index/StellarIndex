package explorer

import (
	"encoding/csv"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

// CSVPage is one page of a list endpoint rendered as text/csv. The JSON
// envelope's fields outside the rows travel as response headers: the next
// page as a Link rel="next", true flags (by their JSON names) in
// X-StellarIndex-Flags, and any other field in headers.
type CSVPage struct {
	Columns    []string
	Rows       [][]string
	NextCursor string
	Flags      []string
	Headers    map[string]string
}

// NegotiateCSV reports whether the request asked for text/csv, and marks
// the response as varying on Accept either way so no cache serves one
// representation to a client that asked for the other.
func NegotiateCSV(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Accept")
	return prefersCSV(r.Header.Get("Accept"))
}

// jsonMediaRank orders the media ranges that admit JSON by specificity.
var jsonMediaRank = map[string]int{"*/*": 1, "application/*": 2, "application/json": 3}

// prefersCSV is true when text/csv outranks JSON in accept. JSON's rank is
// its most specific match (application/json, then application/*, then */*);
// a tie with a wildcard goes to the type the client named.
func prefersCSV(accept string) bool {
	csvQ, jsonQ, jsonRank := -1.0, -1.0, 0
	for _, part := range strings.Split(accept, ",") {
		mt, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		q := 1.0
		if v, ok := params["q"]; ok {
			if q, err = strconv.ParseFloat(v, 64); err != nil {
				continue
			}
		}
		rank := jsonMediaRank[mt]
		switch {
		case mt == "text/csv":
			csvQ = max(csvQ, q)
		case rank > jsonRank:
			jsonQ, jsonRank = q, rank
		}
	}
	return csvQ > 0 && (csvQ > jsonQ || (csvQ == jsonQ && jsonRank < 3))
}

// WriteCSVPage writes p with a header row. Every cell passes through
// csvSafeCell, so no caller can emit a spreadsheet formula. The export is
// never stored in a shared cache: the URL is the JSON one, and a CDN that
// ignores Vary would otherwise hand it to JSON clients.
func WriteCSVPage(w http.ResponseWriter, r *http.Request, p CSVPage) error {
	h := w.Header()
	h.Set("Content-Type", "text/csv; charset=utf-8")
	h.Set("Cache-Control", "private, no-store")
	if p.NextCursor != "" {
		q := r.URL.Query()
		q.Set("cursor", p.NextCursor)
		next := url.URL{Path: r.URL.Path, RawQuery: q.Encode()}
		h.Set("Link", "<"+next.String()+`>; rel="next"`)
	}
	if len(p.Flags) > 0 {
		h.Set("X-StellarIndex-Flags", strings.Join(p.Flags, ", "))
	}
	for k, v := range p.Headers {
		if v = headerSafe(v); v != "" {
			h.Set(k, v)
		}
	}
	w.WriteHeader(http.StatusOK)
	cw := csv.NewWriter(w)
	if err := cw.Write(p.Columns); err != nil {
		return err
	}
	cells := make([]string, len(p.Columns))
	for _, row := range p.Rows {
		for i, c := range row {
			cells[i] = csvSafeCell(c)
		}
		if err := cw.Write(cells[:len(row)]); err != nil {
			return err
		}
	}
	cw.Flush()
	return cw.Error()
}

// asciiFold spells the non-ASCII punctuation a served note uses in ASCII.
var asciiFold = map[rune]string{
	'\u2010': "-", '\u2011': "-", '\u2012': "-", '\u2013': "-", '\u2014': "-", '\u2212': "-",
	'\u2018': "'", '\u2019': "'", '\u201C': `"`, '\u201D': `"`,
	'\u2026': "...", '\u2264': "<=", '\u2265': ">=", '\u00A0': " ",
}

// headerSafe renders s as one line of printable ASCII: known punctuation is
// folded, any other non-ASCII rune becomes '?', and CR, LF and every other
// control character become a space (runs collapsed), so no value can split
// or smuggle a header.
func headerSafe(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		case asciiFold[r] != "":
			b.WriteString(asciiFold[r])
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			b.WriteByte(' ')
		default:
			b.WriteByte('?')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// plainNumber is a decimal that a spreadsheet reads as a number, not a
// formula, so a negative amount keeps its exact text.
var plainNumber = regexp.MustCompile(`^-?[0-9]+(\.[0-9]+)?$`)

// csvSafeCell neutralises CSV injection (OWASP): a cell a spreadsheet would
// evaluate as a formula gets a leading apostrophe. Asset codes, memos and
// attribute values are attacker-chosen.
func csvSafeCell(s string) string {
	if s == "" || plainNumber.MatchString(s) {
		return s
	}
	switch s[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	for _, wide := range []string{"＝", "＋", "－", "＠"} {
		if strings.HasPrefix(s, wide) {
			return "'" + s
		}
	}
	return s
}
