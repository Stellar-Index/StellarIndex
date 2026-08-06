package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// DirectoryReader resolves curated third-party address labels from the
// `account_directory` table (migration 0136; synced from the
// MIT-licensed stellar-expert/public-directory by `stellarindex-ops
// directory-sync`). Production impl is *timescale.Store.
type DirectoryReader interface {
	DirectoryEntryByAddress(ctx context.Context, address string) (timescale.DirectoryEntry, bool, error)
	DirectoryEntriesByAddresses(ctx context.Context, addresses []string) (map[string]timescale.DirectoryEntry, error)
}

// DirectoryInfoV is the wire form of one directory label. These are
// DISPLAY labels with third-party attribution (`source` names the
// upstream set) — they are not a verification signal, and listing is
// not endorsement. Tags follow the upstream registry (#exchange,
// #anchor, #issuer, #wallet, #custodian, #sdf, #memo-required,
// #malicious, #unsafe, …); clients should treat `malicious`/`unsafe`
// as warnings worth surfacing prominently.
type DirectoryInfoV struct {
	Name   string   `json:"name"`
	Domain string   `json:"domain,omitempty"`
	Tags   []string `json:"tags"`
	Source string   `json:"source"`
}

func dirInfoV(e timescale.DirectoryEntry) *DirectoryInfoV {
	tags := e.Tags
	if tags == nil {
		tags = []string{}
	}
	return &DirectoryInfoV{Name: e.Name, Domain: e.Domain, Tags: tags, Source: e.Source}
}

// directoryFor best-effort resolves one address's label. nil when no
// reader is wired, the address isn't listed (the overwhelmingly common
// case), or the read fails — a directory outage must never fail the
// account/contract view it decorates.
func (h *Handler) directoryFor(ctx context.Context, address string) *DirectoryInfoV {
	if h.Directory == nil {
		return nil
	}
	e, ok, err := h.Directory.DirectoryEntryByAddress(ctx, address)
	if err != nil {
		h.Logger.Warn("directory lookup failed", "address", address, "err", err)
		return nil
	}
	if !ok {
		return nil
	}
	return dirInfoV(e)
}

// directoryBatchMax bounds GET /v1/directory's address list. 100 covers
// a full explorer table page of counterparties in one round trip while
// keeping the ANY($1) read trivially cheap.
const directoryBatchMax = 100

// DirectoryLookupView is the wire response for GET /v1/directory:
// entries keyed by address, PRESENT addresses only — an address absent
// from the map has no label. `entries` is always an object (never
// null) so clients can index without a presence check.
type DirectoryLookupView struct {
	Entries map[string]DirectoryInfoV `json:"entries"`
}

// DirectoryLookup serves GET /v1/directory?addresses=G…,C…,… — batch
// label resolution so a client can decorate an address list (an
// operations table's counterparties, a signer list) in one request.
func (h *Handler) DirectoryLookup(w http.ResponseWriter, r *http.Request) {
	if h.Directory == nil {
		h.unavailable(w, r)
		return
	}
	raw := r.URL.Query().Get("addresses")
	if raw == "" {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-addresses",
			"Missing addresses", http.StatusBadRequest,
			"pass ?addresses= as a comma-separated list of G/C strkeys (max "+strconv.Itoa(directoryBatchMax)+")")
		return
	}
	parts := strings.Split(raw, ",")
	if len(parts) > directoryBatchMax {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-addresses",
			"Too many addresses", http.StatusBadRequest,
			"at most "+strconv.Itoa(directoryBatchMax)+" addresses per request")
		return
	}
	addresses := make([]string, 0, len(parts))
	for _, p := range parts {
		a := strings.TrimSpace(p)
		if a == "" {
			continue
		}
		if !canonical.IsAccountID(a) && !canonical.IsContractID(a) {
			h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-addresses",
				"Invalid address", http.StatusBadRequest,
				"not a checksum-valid G/C strkey: "+a)
			return
		}
		addresses = append(addresses, a)
	}
	if len(addresses) == 0 {
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/invalid-addresses",
			"Missing addresses", http.StatusBadRequest,
			"pass ?addresses= as a comma-separated list of G/C strkeys (max "+strconv.Itoa(directoryBatchMax)+")")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), explorerReadTimeout)
	defer cancel()

	found, err := h.Directory.DirectoryEntriesByAddresses(ctx, addresses)
	if err != nil {
		if h.ClientAborted(r, err) {
			return
		}
		h.Logger.Error("directory batch lookup failed", "err", err, "n", len(addresses))
		h.WriteProblem(w, r, "https://api.stellarindex.io/errors/internal",
			"Internal error", http.StatusInternalServerError, "")
		return
	}
	out := DirectoryLookupView{Entries: make(map[string]DirectoryInfoV, len(found))}
	for addr, e := range found {
		out.Entries[addr] = *dirInfoV(e)
	}
	h.WriteJSON(w, out, false)
}
