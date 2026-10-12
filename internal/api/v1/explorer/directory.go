package explorer

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// DirectoryReader resolves curated third-party address labels from the `account_directory` table (synced by `stellarindex-ops directory-
// sync`); production impl is *timescale.Store.
type DirectoryReader interface {
	DirectoryEntryByAddress(ctx context.Context, address string) (timescale.DirectoryEntry, bool, error)
	DirectoryEntriesByAddresses(ctx context.Context, addresses []string) (map[string]timescale.DirectoryEntry, error)
}

// DirectoryInfoV is the wire form of one directory label: a DISPLAY label with third-party `source` attribution, not a verification
// signal; clients should surface `malicious`/`unsafe` tags prominently.
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

// directoryFor resolves one address's label, reporting whether the read answered. ok=false is NOT "unlisted": a #malicious/#unsafe label
// may exist unseen, so the caller must say so on the wire. A directory outage never fails the view it decorates.
func (h *Handler) directoryFor(ctx context.Context, address string) (*DirectoryInfoV, bool) {
	if h.Directory == nil {
		return nil, true
	}
	e, found, err := h.Directory.DirectoryEntryByAddress(ctx, address)
	if err != nil {
		h.Logger.Warn("directory lookup failed", "address", address, "err", err)
		return nil, false
	}
	if !found {
		return nil, true
	}
	return dirInfoV(e), true
}

// directoryBatchMax bounds GET /v1/directory's address list: 100 covers a full explorer table page in one round trip while keeping
// ANY($1) cheap.
const directoryBatchMax = 100

// DirectoryLookupView is GET /v1/directory: entries keyed by address, PRESENT addresses only; `entries` is always an object, never null.
type DirectoryLookupView struct {
	Entries map[string]DirectoryInfoV `json:"entries"`
}

// DirectoryLookup serves GET /v1/directory?addresses=G...,C...: batch label resolution to decorate an address list in one request.
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
		if retryableColdMiss(ctx, err) {
			h.Logger.Warn("explorer directory lookup deadline exceeded", "n", len(addresses))
			h.writeRetryable(w, r, err, "https://api.stellarindex.io/errors/directory-timeout",
				"Directory lookup timed out")
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
	_, stale, _ := h.LakeWatermark(ctx)
	h.WriteJSON(w, out, stale)
}
