package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

// RILQueueAdapterEvidence identifies the exact bounded pending-183 correction.
// The original migration artifact remains immutable; this digest covers the
// complete effective SQL after the source-bound guard replacement.
func RILQueueAdapterEvidence() map[string]any {
	return map[string]any{"id": "ril-183-worker-policy-admission", "version": 1,
		"migration":            "183_ril_queue_proof_expiry.sql",
		"original_sha256":      "a86e7b7427aa844258114824ff044879356e911a8e8e5597db05a8d1259cf874",
		"effective_sql_sha256": "57ca78d44175b5e0f73fed5c8a69a7d35cf6a9bd4266a558fdb32e155717e093"}
}

// SubstrateAbsenceAdapterEvidence identifies the exact bounded pending-190
// correction. The shipped migration patches the insert validator by exact text
// match, but production created that function from a CRLF migration file, so
// its stored body carries \r\n and no LF target matched (Homelab 0.48.3,
// 2026-11-10: "failed-head admission patch target is not at the expected
// version"). The correction folds the read definition to LF before patching.
func SubstrateAbsenceAdapterEvidence() map[string]any {
	return map[string]any{"id": "substrate-190-crlf-function-definition", "version": 1,
		"migration":            "190_substrate_failed_provision_guest_absence.sql",
		"original_sha256":      "363a8b3c19990a54e0ffdd04c1f5713bae8670951982b8e3db0ccd4f616e2128",
		"effective_sql_sha256": "0a9571d38c70c70b01edfa56aa6ca5c403ce3f4241061048a98bce50cf71ddf4"}
}

// executePendingMigration is shared by the normal and exact forward-batch
// engines. The caller has already excluded committed ledger entries and owns
// the migration transaction; denial rolls back without recording this version.
func executePendingMigration(ctx context.Context, tx *sql.Tx, version string, body []byte) error {
	if version == "168_household_projection_bindings.sql" {
		// Preserve the published migration and its manifest checksum. Its role
		// creation is NOLOGIN, but its existing-role branch must not adopt a
		// usable credential into the cross-tenant capability. Activation belongs
		// to the separately governed private worker, never to schema application.
		// Use the same canonical LF identity as forwardBatchBodies on Windows.
		canonical := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		if fmt.Sprintf("%x", sha256.Sum256(canonical)) != "5a5b05d69c313258a46d103862ea005441388527d5136d935d5e37ef38d5a179" {
			return fmt.Errorf("household migration source binding differs")
		}
		var canLogin bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_catalog.pg_roles
			WHERE rolname = 'techstack_household_worker' AND rolcanlogin
		)`).Scan(&canLogin); err != nil {
			return fmt.Errorf("household migration role posture unavailable: %w", err)
		}
		if canLogin {
			return fmt.Errorf("household migration refuses an existing login credential")
		}
	}
	if version == "183_ril_queue_proof_expiry.sql" {
		// Keep the shipped SQL and forward manifest immutable. The original
		// guard rejected every restrictive read policy, including the two
		// source-owned worker ceilings that are true for the migration session.
		// Only their exact inspected definitions are admitted; any changed or
		// additional restrictive policy still refuses the snapshot transaction.
		canonical := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
		if fmt.Sprintf("%x", sha256.Sum256(canonical)) != "a86e7b7427aa844258114824ff044879356e911a8e8e5597db05a8d1259cf874" {
			return fmt.Errorf("RIL queue migration source binding differs")
		}
		const original = `((NOT p.polpermissive AND p.polcmd IN ('r','*')) OR p.polname='techstack_ril_queue_snapshot')`
		const corrected = `(p.polname='techstack_ril_queue_snapshot' OR (NOT p.polpermissive AND p.polcmd IN ('r','*') AND NOT (p.polcmd='*' AND p.polroles=ARRAY[0::oid] AND session_user NOT IN ('techstack_backup_worker','techstack_household_worker') AND ((p.polname='backup_worker_typed_commands' AND pg_get_expr(p.polqual,p.polrelid) IS NOT DISTINCT FROM $ril_policy$((SESSION_USER <> 'techstack_backup_worker'::name) OR ((command_family = 'stackkit'::text) AND ((command_json ->> 'operation'::text) = 'STACKKIT_OPERATION_BACKUP_RUN'::text)))$ril_policy$ AND pg_get_expr(p.polwithcheck,p.polrelid) IS NOT DISTINCT FROM $ril_policy$((SESSION_USER <> 'techstack_backup_worker'::name) OR ((command_family = 'stackkit'::text) AND ((command_json ->> 'operation'::text) = 'STACKKIT_OPERATION_BACKUP_RUN'::text)))$ril_policy$) OR (p.polname='household_worker_command_ceiling' AND pg_get_expr(p.polqual,p.polrelid) IS NOT DISTINCT FROM $ril_policy$((SESSION_USER <> 'techstack_household_worker'::name) OR ((command_family = 'stackkit'::text) AND ((command_json ->> 'operation'::text) = ANY (ARRAY['STACKKIT_OPERATION_HOUSEHOLD_PROJECT'::text, 'STACKKIT_OPERATION_HOUSEHOLD_LIST'::text, 'STACKKIT_OPERATION_HOUSEHOLD_REMOVE'::text]))))$ril_policy$ AND pg_get_expr(p.polwithcheck,p.polrelid) IS NOT DISTINCT FROM $ril_policy$((SESSION_USER <> 'techstack_household_worker'::name) OR ((command_family = 'stackkit'::text) AND ((command_json ->> 'operation'::text) = ANY (ARRAY['STACKKIT_OPERATION_HOUSEHOLD_PROJECT'::text, 'STACKKIT_OPERATION_HOUSEHOLD_LIST'::text, 'STACKKIT_OPERATION_HOUSEHOLD_REMOVE'::text]))))$ril_policy$)))))`
		if bytes.Count(canonical, []byte(original)) != 1 {
			return fmt.Errorf("RIL queue migration guard differs")
		}
		body = bytes.Replace(canonical, []byte(original), []byte(corrected), 1)
		// The source revision and this digest bind the exact effective SQL in the native receipt.
		if fmt.Sprintf("%x", sha256.Sum256(body)) != "57ca78d44175b5e0f73fed5c8a69a7d35cf6a9bd4266a558fdb32e155717e093" {
			return fmt.Errorf("RIL queue effective migration binding differs")
		}
	}
	if version == "190_substrate_failed_provision_guest_absence.sql" {
		corrected, err := substrateAbsenceEffectiveBody(body)
		if err != nil {
			return err
		}
		body = corrected
	}
	_, err := tx.ExecContext(ctx, string(body))
	return err
}

// substrateAbsenceEffectiveBody keeps the shipped 190 SQL and forward manifest
// immutable and corrects only the line ending of the function text it
// patches: the read definition is folded to LF, so the four exact patch
// targets match a CRLF-created function as well as an LF one and still refuse
// any other definition.
func substrateAbsenceEffectiveBody(body []byte) ([]byte, error) {
	canonical := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if fmt.Sprintf("%x", sha256.Sum256(canonical)) != "363a8b3c19990a54e0ffdd04c1f5713bae8670951982b8e3db0ccd4f616e2128" {
		return nil, fmt.Errorf("substrate absence migration source binding differs")
	}
	const original = "    ) INTO function_definition;\n"
	const corrected = original + "    function_definition := pg_catalog.replace(function_definition, E'\\r\\n', E'\\n');\n"
	if bytes.Count(canonical, []byte(original)) != 1 {
		return nil, fmt.Errorf("substrate absence migration definition read differs")
	}
	effective := bytes.Replace(canonical, []byte(original), []byte(corrected), 1)
	if fmt.Sprintf("%x", sha256.Sum256(effective)) != "0a9571d38c70c70b01edfa56aa6ca5c403ce3f4241061048a98bce50cf71ddf4" {
		return nil, fmt.Errorf("substrate absence effective migration binding differs")
	}
	return effective, nil
}
