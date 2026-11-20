package household

import (
	"context"
	"database/sql"

	"github.com/kombifyio/homelab/internal/systemdb"
)

// homeGraphCloudMigration is the Cloud migration that grants the household
// system role SELECT on the home graph tables (D-2026-11-09-03, Cloud #2770).
const homeGraphCloudMigration = "000404_home_graph.up.sql"

// homeGraphCloudTables are readable by the household system role once
// homeGraphCloudMigration is applied; the worker reads them to project the
// home graph to the owner's node.
var homeGraphCloudTables = []string{"home_graphs", "home_floors", "home_rooms", "home_devices", "home_assignments"}

// This is a startup assertion of the two migration-owned capabilities, not a
// grant installer. Unexpected inherited/PUBLIC/column rights and any
// SECURITY DEFINER routine beyond the expected ones refuse execution; the
// worker cannot repair or widen its own authority.
func verifyHouseholdPrivileges(ctx context.Context, pool *sql.DB, cloud bool) error {
	tables := map[string]string{"kombify_ctx.role_class": "SELECT"}
	columns := map[string]string{}
	functions := map[string]bool{"kombify_ctx.verified_context()": true, "kombify_ctx.subject()": true, "kombify_ctx.org_id()": true}
	if cloud {
		tables["public.kombify_db_migrations"] = "SELECT"
		for _, name := range []string{"spaces", "space_members", "space_invites", "space_member_grants", "household_lifecycle_receipts"} {
			tables["public."+name] = "SELECT"
		}
		tables["public.household_lifecycle_outbox"] = "SELECT,UPDATE"
		tables["public.household_node_projections"] = "SELECT,INSERT,UPDATE"
		columns["public.users"] = "id,auth_subject,email,name,avatar_url"
		functions["public.household_reduce_lifecycle(integer)"] = true
		// Cloud and Homelab release independently: the expected set follows
		// the Cloud ledger, so the worker starts before and after 000404.
		homeGraph, err := cloudMigrationApplied(ctx, pool, homeGraphCloudMigration)
		if err != nil {
			return err
		}
		if homeGraph {
			for _, name := range homeGraphCloudTables {
				tables["public."+name] = "SELECT"
			}
		}
	} else {
		for _, name := range []string{"schema_migrations", "techstack_memberships", "techstack_tenants", "homelabs", "stacks", "workers", "servers"} {
			tables["public."+name] = "SELECT"
		}
		tables["public.household_projection_bindings"] = "SELECT,INSERT,UPDATE"
		tables["public.household_projection_attempts"] = "SELECT,INSERT,UPDATE"
		tables["public.household_identity_refs"] = "SELECT,INSERT,UPDATE,DELETE"
		tables["public.typed_agent_commands"] = "SELECT,INSERT,UPDATE,DELETE"
		columns["public.services"] = "tenant_id,stack_id,service_key,name,management_state"
	}
	return systemdb.VerifyPrivileges(ctx, pool, tables, columns, functions)
}

// cloudMigrationApplied reports whether the Cloud ledger records version as
// cleanly applied. A read failure refuses startup rather than guessing.
func cloudMigrationApplied(ctx context.Context, pool *sql.DB, version string) (bool, error) {
	var applied bool
	err := pool.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM public.kombify_db_migrations WHERE version=$1 AND NOT dirty AND applied_at IS NOT NULL)`, version).Scan(&applied)
	if err != nil {
		return false, &StartupRefusal{Stage: "cloud-migration", Reason: "Cloud migration ledger unavailable"}
	}
	return applied, nil
}
