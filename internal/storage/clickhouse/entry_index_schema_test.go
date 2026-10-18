// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"strings"
	"testing"
)

// TestEntryIndexRolesAreInTheSchemaEnum: a role the builder writes but the Enum8 lacks fails every window at INSERT,
// as the signer role did on r1 the moment its binary deployed.
func TestEntryIndexRolesAreInTheSchemaEnum(t *testing.T) {
	roles := []string{EntryIndexRoleOwner, EntryIndexRoleClaimant, EntryIndexRoleSponsor, EntryIndexRoleSigner}
	for _, file := range []string{"tier1_schema.sql", "entry_index.sql"} {
		ddl := createDDL(t, file, "stellar.account_entry_ledgers")
		if ddl == "" {
			t.Fatalf("%s: no CREATE for stellar.account_entry_ledgers", file)
		}
		for _, r := range roles {
			if !strings.Contains(ddl, "'"+r+"'") {
				t.Errorf("%s: account_entry_ledgers.role enum lacks %q", file, r)
			}
		}
	}
}
