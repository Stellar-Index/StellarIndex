package controlwiring

import (
	"fmt"
	"testing"
)

// The -chunks pre-flights read current_setting('data_directory') as the stellarindex role; only
// pg_read_all_settings lets a non-superuser do that. The API role must never get a built-in role.
func TestPostgresTasks_ReadSettingsGrantedToStellarindexRoleOnly(t *testing.T) {
	var grants []map[string]any
	roleAt, grantAt := -1, -1
	for i, n := range walTasks(t) {
		m := walTaskMap(t, n)
		if m["name"] == "Create stellarindex Postgres role" {
			roleAt = i
		}
		if mod, ok := m["community.postgresql.postgresql_membership"].(map[string]any); ok {
			grants = append(grants, mod)
			grantAt = i
		}
	}
	if roleAt < 0 || grantAt < roleAt {
		t.Fatalf("grant task at %d, role creation at %d: the grant must follow the role it targets", grantAt, roleAt)
	}
	if len(grants) != 1 {
		t.Fatalf("%d postgresql_membership tasks in %s, want exactly 1", len(grants), walPostgresTasks)
	}
	g := grants[0]
	if got := fmt.Sprint(g["groups"]); got != "[pg_read_all_settings]" {
		t.Errorf("groups = %s, want [pg_read_all_settings]", got)
	}
	if got := fmt.Sprint(g["target_roles"]); got != "[{{ postgres_user_stellarindex }}]" {
		t.Errorf("target_roles = %s, want only the stellarindex role", got)
	}
	if g["state"] != "present" {
		t.Errorf("state = %v, want present (exact would revoke other memberships)", g["state"])
	}
}
