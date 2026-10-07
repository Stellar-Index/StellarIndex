// Package holds is the operator "under review" hold list: a TOML file the
// API polls and swaps in atomically, so a hold can be added or lifted
// without a restart. A matching hold marks supply, balance and holder
// responses; it never changes the numbers themselves.
package holds

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Hold marks responses that match ALL of its selectors.
type Hold struct {
	// Asset is an asset id; its XLM alias family and a classic asset's
	// derived SAC also match.
	Asset string `toml:"asset"`
	// ContractID is a Soroban contract strkey (C…).
	ContractID string `toml:"contract_id"`
	// LedgerFrom/LedgerTo bound the response's as_of_ledger; LedgerTo 0 is open-ended.
	LedgerFrom uint32 `toml:"ledger_from"`
	LedgerTo   uint32 `toml:"ledger_to"`
	Reason     string `toml:"reason"`
}

type file struct {
	Hold []Hold `toml:"hold"`
}

// Subject is what a response is about, extracted by the caller.
type Subject struct {
	// Assets are every asset or contract identifier the response names.
	Assets []string
	// Ledger is the response's as_of_ledger; 0 means unknown.
	Ledger uint32
}

// Parse decodes and validates a hold file. Unknown keys are an error so a
// typo cannot silently turn a hold into a no-op.
func Parse(doc []byte) ([]Hold, error) {
	var f file
	meta, err := toml.NewDecoder(bytes.NewReader(doc)).Decode(&f)
	if err != nil {
		return nil, fmt.Errorf("holds: decode: %w", err)
	}
	if undec := meta.Undecoded(); len(undec) > 0 {
		return nil, fmt.Errorf("holds: unknown keys %v", undec)
	}
	for i, h := range f.Hold {
		if err := h.validate(); err != nil {
			return nil, fmt.Errorf("holds: hold[%d]: %w", i, err)
		}
	}
	return f.Hold, nil
}

func (h Hold) validate() error {
	if h.Reason == "" {
		return errors.New("reason is required")
	}
	if h.Asset == "" && h.ContractID == "" && h.LedgerFrom == 0 && h.LedgerTo == 0 {
		return errors.New("needs at least one of asset, contract_id, ledger_from, ledger_to")
	}
	if h.Asset != "" {
		if _, err := canonical.ParseAsset(h.Asset); err != nil {
			return fmt.Errorf("asset: %w", err)
		}
	}
	if h.ContractID != "" && (len(h.ContractID) != 56 || h.ContractID[0] != 'C') {
		return fmt.Errorf("contract_id %q is not a contract strkey", h.ContractID)
	}
	if h.LedgerTo != 0 && h.LedgerTo < h.LedgerFrom {
		return errors.New("ledger_to is before ledger_from")
	}
	return nil
}

func (h Hold) matches(sub Subject) bool {
	forms := subjectForms(sub.Assets)
	if h.Asset != "" && !overlaps(forms, h.Asset) {
		return false
	}
	if h.ContractID != "" && !overlaps(forms, h.ContractID) {
		return false
	}
	if h.LedgerFrom != 0 || h.LedgerTo != 0 {
		if sub.Ledger == 0 || sub.Ledger < h.LedgerFrom || (h.LedgerTo != 0 && sub.Ledger > h.LedgerTo) {
			return false
		}
	}
	return true
}

// identityForms is every spelling of the asset a response could name: the
// parsed id (so "XLM"/"Native" fold to native), its alias family, and the
// derived SAC of a classic asset. An unparseable id stands for itself.
func identityForms(s string) []string {
	a, err := canonical.ParseAsset(s)
	if err != nil {
		return []string{s}
	}
	forms := []string{s, a.String()}
	for _, alias := range canonical.AssetAliases(a) {
		forms = append(forms, alias.String())
	}
	for _, alias := range canonical.AssetAliases(canonical.CanonicalAsset(a)) {
		forms = append(forms, alias.String())
	}
	if a.Type == canonical.AssetClassic {
		if sac, err := a.SacContractID(); err == nil {
			forms = append(forms, sac)
		}
	}
	return forms
}

func subjectForms(assets []string) map[string]struct{} {
	set := map[string]struct{}{}
	for _, s := range assets {
		for _, f := range identityForms(s) {
			set[f] = struct{}{}
		}
	}
	return set
}

// overlaps reports whether any identity form of id is in the subject set.
func overlaps(subject map[string]struct{}, id string) bool {
	for _, f := range identityForms(id) {
		if _, ok := subject[f]; ok {
			return true
		}
	}
	return false
}

// Match returns the first hold covering the subject.
func Match(holds []Hold, sub Subject) (Hold, bool) {
	for _, h := range holds {
		if h.matches(sub) {
			return h, true
		}
	}
	return Hold{}, false
}

// DefaultReloadInterval replaces a non-positive Watch interval, which would
// panic time.NewTicker.
const DefaultReloadInterval = 15 * time.Second

// Watch polls path every interval and hands each successfully parsed list
// to apply. A missing file means no holds; an unreadable, invalid or
// zero-byte file keeps the previous list, so a bad edit cannot silently lift a hold.
// Blocks until ctx is done.
func Watch(ctx context.Context, path string, interval time.Duration, apply func([]Hold), logger *slog.Logger) {
	var last []byte
	reload := func() {
		doc, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
		switch {
		case errors.Is(err, fs.ErrNotExist):
			doc = []byte{}
		case err != nil:
			logger.Warn("holds: read failed, keeping previous list", "path", path, "err", err)
			return
		case len(doc) == 0 && len(last) > 0:
			// An in-place rewrite truncates first; deleting the file is how to lift every hold.
			logger.Warn("holds: empty file, keeping previous list", "path", path)
			return
		}
		if last != nil && bytes.Equal(doc, last) {
			return
		}
		list, err := Parse(doc)
		if err != nil {
			logger.Warn("holds: invalid file, keeping previous list", "path", path, "err", err)
			return
		}
		last = doc
		apply(list)
		logger.Info("holds: reloaded", "path", path, "count", len(list))
	}
	reload()
	if interval <= 0 {
		interval = DefaultReloadInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			reload()
		}
	}
}
