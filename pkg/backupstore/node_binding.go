package backupstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrRecipientSubstituted reports that a node offered a recipient key other than
// the one pinned at enrollment. Re-enrollment is an explicit, separate act.
var ErrRecipientSubstituted = errors.New("managed backup recipient differs from the pinned enrollment")

// ErrNodeBindingMissing reports that the node has issued no binding yet.
var ErrNodeBindingMissing = errors.New("managed backup node binding is not recorded")

// RecipientPin is the node's sealed-delivery recipient as enrolled: the owner
// key that signed the statement, the recipient key's fingerprint and the
// generation sealing binds to.
type RecipientPin struct {
	TenantID    string
	StackID     string
	OwnerKey    string
	OwnerKeyID  string
	Fingerprint string
	Generation  uint64
	SiteRef     string
	NodeRef     string
	PinnedAt    time.Time
}

// NodeBinding holds the opaque references the node issued for its own custody.
// Core compares against them and never derives them.
type NodeBinding struct {
	BackupTargetRef       string
	CustodyAttestationRef string
	RecordedAt            time.Time
}

// PinRecipient records the node's recipient at first enrollment. An identical
// repeat is accepted; any other recipient for the same stack is refused.
func (s *PostgresCustodyStore) PinRecipient(ctx context.Context, pin RecipientPin) error {
	if pin.TenantID == "" || pin.StackID == "" || pin.OwnerKey == "" || pin.OwnerKeyID == "" || pin.Fingerprint == "" || pin.Generation == 0 || pin.SiteRef == "" || pin.NodeRef == "" {
		return fmt.Errorf("%w: a complete recipient pin is required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, pin.TenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_stores
			SET recipient_owner_public_key = $3, recipient_owner_key_id = $4, recipient_fingerprint = $5,
				recipient_generation = $6, recipient_site_ref = $7, recipient_node_ref = $8, recipient_pinned_at = COALESCE(recipient_pinned_at, now())
			WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
			  AND (recipient_fingerprint IS NULL
			       OR (recipient_owner_public_key = $3 AND recipient_owner_key_id = $4 AND recipient_fingerprint = $5
			           AND recipient_generation = $6 AND recipient_site_ref = $7 AND recipient_node_ref = $8))
		`, pin.TenantID, pin.StackID, pin.OwnerKey, pin.OwnerKeyID, pin.Fingerprint, int64(pin.Generation), pin.SiteRef, pin.NodeRef) // #nosec G115 -- generation is a small counter.
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed == 1 {
			return nil
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT true FROM stack_backup_stores WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL`, pin.TenantID, pin.StackID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		} else if err != nil {
			return err
		}
		return ErrRecipientSubstituted
	})
}

// Recipient returns the pinned recipient, if the node has been enrolled.
func (s *PostgresCustodyStore) Recipient(ctx context.Context, tenantID, stackID string) (RecipientPin, bool, error) {
	tenantID, stackID = strings.TrimSpace(tenantID), strings.TrimSpace(stackID)
	if tenantID == "" || stackID == "" {
		return RecipientPin{}, false, fmt.Errorf("%w: tenant id and stack id are required", ErrInvalidCustody)
	}
	var pin RecipientPin
	var key, keyID, fingerprint, site, node sql.NullString
	var generation sql.NullInt64
	var pinned sql.NullTime
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT recipient_owner_public_key, recipient_owner_key_id, recipient_fingerprint, recipient_generation,
				recipient_site_ref, recipient_node_ref, recipient_pinned_at
			FROM stack_backup_stores WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
		`, tenantID, stackID).Scan(&key, &keyID, &fingerprint, &generation, &site, &node, &pinned)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		}
		return err
	})
	if err != nil {
		return RecipientPin{}, false, err
	}
	if !fingerprint.Valid {
		return RecipientPin{}, false, nil
	}
	pin = RecipientPin{TenantID: tenantID, StackID: stackID, OwnerKey: key.String, OwnerKeyID: keyID.String, Fingerprint: fingerprint.String,
		Generation: uint64(generation.Int64), SiteRef: site.String, NodeRef: node.String, PinnedAt: pinned.Time} // #nosec G115 -- constrained positive by the table.
	return pin, true, nil
}

// RecordNodeBinding stores the references the node issued. A second record for
// the same stack must name the same target and custody: the node refuses a
// replacement, so a different value is a defect, not an update.
func (s *PostgresCustodyStore) RecordNodeBinding(ctx context.Context, tenantID, stackID string, binding NodeBinding) error {
	tenantID, stackID = strings.TrimSpace(tenantID), strings.TrimSpace(stackID)
	if tenantID == "" || stackID == "" || binding.BackupTargetRef == "" || binding.CustodyAttestationRef == "" {
		return fmt.Errorf("%w: a complete node binding is required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_stores
			SET node_backup_target_ref = $3, node_custody_attestation_ref = $4, node_binding_recorded_at = COALESCE(node_binding_recorded_at, now())
			WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL AND recipient_fingerprint IS NOT NULL
			  AND (node_backup_target_ref IS NULL OR (node_backup_target_ref = $3 AND node_custody_attestation_ref = $4))
		`, tenantID, stackID, binding.BackupTargetRef, binding.CustodyAttestationRef)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("managed backup node binding differs from the one already recorded or the recipient is not enrolled")
		}
		return nil
	})
}

// NodeBinding returns the references the node issued for its own custody.
func (s *PostgresCustodyStore) NodeBinding(ctx context.Context, tenantID, stackID string) (NodeBinding, error) {
	tenantID, stackID = strings.TrimSpace(tenantID), strings.TrimSpace(stackID)
	if tenantID == "" || stackID == "" {
		return NodeBinding{}, fmt.Errorf("%w: tenant id and stack id are required", ErrInvalidCustody)
	}
	var target, custody sql.NullString
	var recorded sql.NullTime
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT node_backup_target_ref, node_custody_attestation_ref, node_binding_recorded_at
			FROM stack_backup_stores WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
		`, tenantID, stackID).Scan(&target, &custody, &recorded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		}
		return err
	})
	if err != nil {
		return NodeBinding{}, err
	}
	if !target.Valid {
		return NodeBinding{}, ErrNodeBindingMissing
	}
	return NodeBinding{BackupTargetRef: target.String, CustodyAttestationRef: custody.String, RecordedAt: recorded.Time}, nil
}

// ReplacementRecipientPin is the destination node's staged recipient enrollment
// for a customer-accepted, zero-charge Memory Cube S support replacement.
type ReplacementRecipientPin struct {
	RecipientPin
	ReplacementID                 string
	DestinationLeaseID            string
	DestinationServerID           string
	DestinationResourceGeneration string
}

// StageReplacementRecipient stages a replacement destination node's recipient
// key in stack_backup_replacement_handoffs without disturbing the source node's
// active pin in stack_backup_stores. Identical retries for the same
// replacement_id succeed; any key or identity change returns ErrRecipientSubstituted.
func (s *PostgresCustodyStore) StageReplacementRecipient(ctx context.Context, pin ReplacementRecipientPin) error {
	pin.TenantID = strings.TrimSpace(pin.TenantID)
	pin.StackID = strings.TrimSpace(pin.StackID)
	pin.ReplacementID = strings.TrimSpace(pin.ReplacementID)
	pin.DestinationLeaseID = strings.TrimSpace(pin.DestinationLeaseID)
	pin.DestinationServerID = strings.TrimSpace(pin.DestinationServerID)
	pin.DestinationResourceGeneration = strings.TrimSpace(pin.DestinationResourceGeneration)
	if pin.TenantID == "" || pin.StackID == "" || pin.ReplacementID == "" ||
		pin.DestinationLeaseID == "" || pin.DestinationServerID == "" || pin.DestinationResourceGeneration == "" ||
		pin.OwnerKey == "" || pin.OwnerKeyID == "" || pin.Fingerprint == "" || pin.Generation == 0 || pin.SiteRef == "" || pin.NodeRef == "" {
		return fmt.Errorf("%w: a complete replacement recipient pin is required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, pin.TenantID, func(tx *sql.Tx) error {
		var exists bool
		if err := tx.QueryRowContext(ctx, `
			SELECT true FROM stack_backup_stores
			WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
		`, pin.TenantID, pin.StackID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		} else if err != nil {
			return err
		}

		res, err := tx.ExecContext(ctx, `
			INSERT INTO stack_backup_replacement_handoffs (
				tenant_id, stack_id, replacement_id,
				destination_lease_id, destination_server_id, destination_resource_generation_id,
				recipient_owner_public_key, recipient_owner_key_id, recipient_fingerprint,
				recipient_generation, recipient_site_ref, recipient_node_ref
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
			ON CONFLICT (tenant_id, stack_id, replacement_id) DO NOTHING
		`, pin.TenantID, pin.StackID, pin.ReplacementID,
			pin.DestinationLeaseID, pin.DestinationServerID, pin.DestinationResourceGeneration,
			pin.OwnerKey, pin.OwnerKeyID, pin.Fingerprint,
			int64(pin.Generation), pin.SiteRef, pin.NodeRef) // #nosec G115 -- generation is a small counter.
		if err != nil {
			return err
		}
		if changed, _ := res.RowsAffected(); changed == 1 {
			return nil
		}

		var existing ReplacementRecipientPin
		var gen int64
		var status string
		if err := tx.QueryRowContext(ctx, `
			SELECT destination_lease_id, destination_server_id, destination_resource_generation_id,
			       recipient_owner_public_key, recipient_owner_key_id, recipient_fingerprint,
			       recipient_generation, recipient_site_ref, recipient_node_ref, status
			FROM stack_backup_replacement_handoffs
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3
		`, pin.TenantID, pin.StackID, pin.ReplacementID).Scan(
			&existing.DestinationLeaseID, &existing.DestinationServerID, &existing.DestinationResourceGeneration,
			&existing.OwnerKey, &existing.OwnerKeyID, &existing.Fingerprint,
			&gen, &existing.SiteRef, &existing.NodeRef, &status,
		); err != nil {
			return err
		}
		if status != "staged" {
			return fmt.Errorf("%w: replacement handoff %q is %s", ErrInvalidCustody, pin.ReplacementID, status)
		}
		if existing.DestinationLeaseID == pin.DestinationLeaseID &&
			existing.DestinationServerID == pin.DestinationServerID &&
			existing.DestinationResourceGeneration == pin.DestinationResourceGeneration &&
			existing.OwnerKey == pin.OwnerKey &&
			existing.OwnerKeyID == pin.OwnerKeyID &&
			existing.Fingerprint == pin.Fingerprint &&
			uint64(gen) == pin.Generation && // #nosec G115 -- constrained positive by the table.
			existing.SiteRef == pin.SiteRef &&
			existing.NodeRef == pin.NodeRef {
			return nil
		}
		return ErrRecipientSubstituted
	})
}

// ReplacementRecipient returns the currently staged recipient pin for a support replacement, if any.
func (s *PostgresCustodyStore) ReplacementRecipient(ctx context.Context, tenantID, stackID, replacementID string) (ReplacementRecipientPin, bool, error) {
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || stackID == "" || replacementID == "" {
		return ReplacementRecipientPin{}, false, fmt.Errorf("%w: tenant id, stack id, and replacement id are required", ErrInvalidCustody)
	}
	var pin ReplacementRecipientPin
	var gen int64
	var found bool
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT destination_lease_id, destination_server_id, destination_resource_generation_id,
			       recipient_owner_public_key, recipient_owner_key_id, recipient_fingerprint,
			       recipient_generation, recipient_site_ref, recipient_node_ref, recipient_pinned_at
			FROM stack_backup_replacement_handoffs
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3 AND status = 'staged'
		`, tenantID, stackID, replacementID).Scan(
			&pin.DestinationLeaseID, &pin.DestinationServerID, &pin.DestinationResourceGeneration,
			&pin.OwnerKey, &pin.OwnerKeyID, &pin.Fingerprint,
			&gen, &pin.SiteRef, &pin.NodeRef, &pin.PinnedAt,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		found = true
		return nil
	})
	if err != nil || !found {
		return ReplacementRecipientPin{}, false, err
	}
	pin.TenantID = tenantID
	pin.StackID = stackID
	pin.ReplacementID = replacementID
	pin.Generation = uint64(gen) // #nosec G115 -- constrained positive by the table.
	return pin, true, nil
}

// RecordReplacementNodeBinding records the destination node's backup-target and
// custody-attestation references on the staged replacement handoff without
// altering stack_backup_stores before cutover.
func (s *PostgresCustodyStore) RecordReplacementNodeBinding(ctx context.Context, tenantID, stackID, replacementID string, binding NodeBinding) error {
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || stackID == "" || replacementID == "" || binding.BackupTargetRef == "" || binding.CustodyAttestationRef == "" {
		return fmt.Errorf("%w: a complete replacement node binding is required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_replacement_handoffs
			SET node_backup_target_ref = $4,
			    node_custody_attestation_ref = $5,
			    node_binding_recorded_at = COALESCE(node_binding_recorded_at, now())
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3 AND status = 'staged'
			  AND (node_backup_target_ref IS NULL OR (node_backup_target_ref = $4 AND node_custody_attestation_ref = $5))
		`, tenantID, stackID, replacementID, binding.BackupTargetRef, binding.CustodyAttestationRef)
		if err != nil {
			return err
		}
		if changed, _ := result.RowsAffected(); changed != 1 {
			return errors.New("managed backup replacement node binding differs from the one already recorded or the replacement handoff is not staged")
		}
		return nil
	})
}

// ReplacementNodeBinding returns the node binding recorded on a staged or
// committed replacement handoff row.
func (s *PostgresCustodyStore) ReplacementNodeBinding(ctx context.Context, tenantID, stackID, replacementID string) (NodeBinding, error) {
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || stackID == "" || replacementID == "" {
		return NodeBinding{}, fmt.Errorf("%w: tenant id, stack id, and replacement id are required", ErrInvalidCustody)
	}
	var target, custody sql.NullString
	var recorded sql.NullTime
	err := s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
			SELECT node_backup_target_ref, node_custody_attestation_ref, node_binding_recorded_at
			FROM stack_backup_replacement_handoffs
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3 AND status IN ('staged', 'committed')
		`, tenantID, stackID, replacementID).Scan(&target, &custody, &recorded)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		}
		return err
	})
	if err != nil {
		return NodeBinding{}, err
	}
	if !target.Valid {
		return NodeBinding{}, ErrNodeBindingMissing
	}
	return NodeBinding{BackupTargetRef: target.String, CustodyAttestationRef: custody.String, RecordedAt: recorded.Time}, nil
}

// CommitReplacementCustody promotes the staged replacement recipient pin and
// node binding into stack_backup_stores once destination writes are observed
// during cutover. Idempotent if already committed.
func (s *PostgresCustodyStore) CommitReplacementCustody(ctx context.Context, tenantID, stackID, replacementID string) error {
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || stackID == "" || replacementID == "" {
		return fmt.Errorf("%w: tenant id, stack id, and replacement id are required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		var ownerKey, ownerKeyID, fingerprint, siteRef, nodeRef, status string
		var gen int64
		var pinnedAt time.Time
		var targetRef, attestationRef sql.NullString
		var bindingAt sql.NullTime
		err := tx.QueryRowContext(ctx, `
			SELECT recipient_owner_public_key, recipient_owner_key_id, recipient_fingerprint,
			       recipient_generation, recipient_site_ref, recipient_node_ref, recipient_pinned_at,
			       node_backup_target_ref, node_custody_attestation_ref, node_binding_recorded_at, status
			FROM stack_backup_replacement_handoffs
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3
			FOR UPDATE
		`, tenantID, stackID, replacementID).Scan(
			&ownerKey, &ownerKeyID, &fingerprint,
			&gen, &siteRef, &nodeRef, &pinnedAt,
			&targetRef, &attestationRef, &bindingAt, &status,
		)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrCustodyNotFound
		}
		if err != nil {
			return err
		}
		if status == "committed" {
			return nil
		}
		if status != "staged" || !targetRef.Valid || !attestationRef.Valid || !bindingAt.Valid {
			return fmt.Errorf("%w: replacement handoff %q is not ready for commit (status=%q)", ErrInvalidCustody, replacementID, status)
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_stores
			SET recipient_owner_public_key = $3,
			    recipient_owner_key_id = $4,
			    recipient_fingerprint = $5,
			    recipient_generation = $6,
			    recipient_site_ref = $7,
			    recipient_node_ref = $8,
			    recipient_pinned_at = $9,
			    node_backup_target_ref = $10,
			    node_custody_attestation_ref = $11,
			    node_binding_recorded_at = $12
			WHERE tenant_id = $1 AND stack_id = $2 AND token_revoked_at IS NULL
		`, tenantID, stackID, ownerKey, ownerKeyID, fingerprint, gen, siteRef, nodeRef, pinnedAt,
			targetRef.String, attestationRef.String, bindingAt.Time)
		if err != nil {
			return err
		}
		if changed, _ := res.RowsAffected(); changed != 1 {
			return ErrCustodyNotFound
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_replacement_handoffs
			SET status = 'committed',
			    committed_at = now()
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3 AND status = 'staged'
		`, tenantID, stackID, replacementID); err != nil {
			return err
		}
		return nil
	})
}

// DiscardReplacementCustody marks any staged replacement handoff as rolled_back
// on pre-cutover rollback, leaving stack_backup_stores intact for the source node.
func (s *PostgresCustodyStore) DiscardReplacementCustody(ctx context.Context, tenantID, stackID, replacementID string) error {
	tenantID = strings.TrimSpace(tenantID)
	stackID = strings.TrimSpace(stackID)
	replacementID = strings.TrimSpace(replacementID)
	if tenantID == "" || stackID == "" || replacementID == "" {
		return fmt.Errorf("%w: tenant id, stack id, and replacement id are required", ErrInvalidCustody)
	}
	return s.withTenant(ctx, tenantID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `
			UPDATE stack_backup_replacement_handoffs
			SET status = 'rolled_back',
			    rolled_back_at = COALESCE(rolled_back_at, now())
			WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3
			  AND status IN ('staged', 'rolled_back')
		`, tenantID, stackID, replacementID)
		if err != nil {
			return err
		}
		if changed, _ := res.RowsAffected(); changed == 0 {
			var status string
			if qErr := tx.QueryRowContext(ctx, `
				SELECT status FROM stack_backup_replacement_handoffs
				WHERE tenant_id = $1 AND stack_id = $2 AND replacement_id = $3
			`, tenantID, stackID, replacementID).Scan(&status); qErr == nil && status == "committed" {
				return fmt.Errorf("%w: cannot discard committed replacement handoff %q", ErrInvalidCustody, replacementID)
			} else if qErr != nil && !errors.Is(qErr, sql.ErrNoRows) {
				return qErr
			}
		}
		return nil
	})
}
