package timescale

import (
	"context"
	"fmt"
)

// ClassicCodeKey names one classic asset spelling under one issuer.
type ClassicCodeKey struct {
	Issuer string
	Code   string
}

// classicCaseVariantsLimit tells none, one and several case spellings apart; nothing needs more.
const classicCaseVariantsLimit = 3

// ClassicCodesFoldingTo returns, per key, up to three classic_assets codes under the key's issuer equal to its code ignoring
// case, the key's own spelling included when it exists; one round trip, bounded by len(keys) issuer-index probes.
func (s *Store) ClassicCodesFoldingTo(ctx context.Context, keys []ClassicCodeKey) (map[ClassicCodeKey][]string, error) {
	out := make(map[ClassicCodeKey][]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	issuers, codes := make([]string, len(keys)), make([]string, len(keys))
	for i, k := range keys {
		issuers[i], codes[i] = k.Issuer, k.Code
	}
	const q = `
        SELECT k.issuer, k.code, ca.code
          FROM unnest($1::text[], $2::text[]) AS k(issuer, code)
         CROSS JOIN LATERAL (
               SELECT c.code
                 FROM classic_assets c
                WHERE c.issuer_g_strkey = k.issuer
                  AND upper(c.code) = upper(k.code)
                ORDER BY c.code
                LIMIT $3
         ) ca
    `
	rows, err := s.db.QueryContext(ctx, q, issuers, codes, classicCaseVariantsLimit)
	if err != nil {
		return nil, fmt.Errorf("timescale: ClassicCodesFoldingTo: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var k ClassicCodeKey
		var code string
		if err := rows.Scan(&k.Issuer, &k.Code, &code); err != nil {
			return nil, fmt.Errorf("timescale: ClassicCodesFoldingTo scan: %w", err)
		}
		out[k] = append(out[k], code)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: ClassicCodesFoldingTo rows: %w", err)
	}
	return out, nil
}
