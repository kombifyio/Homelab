package guardbootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/db/tenantcontext"
	"github.com/kombifyio/homelab/pkg/pairingtoken"
)

// ReenrollCapabilitySource marks a capability minted for the managed Guard
// re-enrollment of an existing lease (executionchannel.Channel.ReenrollGuard).
const ReenrollCapabilitySource = "managed-guard-reenroll"

// reenrollWindow bounds one re-enrollment attempt. The control plane runs the
// installer itself over the execution channel right after minting, so the
// capability needs minutes, not the first-boot window of a fresh VM.
const reenrollWindow = 30 * time.Minute

// reenrollMinRemaining is the least lifetime a capability must have left when
// it is handed out: the installer run it authorizes takes up to 15 minutes,
// so a near-expiry row is never returned.
const reenrollMinRemaining = 20 * time.Minute

// ReenrollRequest names one re-enrollment attempt of a managed node. The
// attempt id is part of the derivation scope, so a repeated call for the same
// attempt reissues the same capability and a new attempt gets a new one.
type ReenrollRequest struct {
	TenantID  string
	LeaseID   string
	StackID   string
	ServerID  string
	AttemptID string
}

// ErrReenrollCapabilitySpent means the capability of this attempt was already
// redeemed or revoked, or has too little lifetime left for an installer run;
// the caller starts a new attempt instead of reusing it.
var ErrReenrollCapabilitySpent = errors.New("guardbootstrap: the re-enrollment capability of this attempt is spent")

// IssueReenrollCapability derives and records the lease-bound pairing
// capability of one re-enrollment attempt and returns the raw capability. It
// carries lease_id like a provisioning capability, so the redemption binds the
// Guard to the lease's existing worker and server (workers.go
// workerStoreIDForLease) and issues a new host-key identity and credential
// epoch for it. The raw value is returned only to the caller that feeds it to
// the installer on the node; the database keeps its hash.
func (i *EnrollmentIssuer) IssueReenrollCapability(ctx context.Context, request ReenrollRequest) (string, error) {
	if i == nil || i.database == nil {
		return "", fmt.Errorf("guardbootstrap: enrolment issuer is not configured")
	}
	tenantID, leaseID := strings.TrimSpace(request.TenantID), strings.TrimSpace(request.LeaseID)
	stackID, attemptID := strings.TrimSpace(request.StackID), strings.TrimSpace(request.AttemptID)
	if tenantID == "" || leaseID == "" || stackID == "" || attemptID == "" {
		return "", fmt.Errorf("guardbootstrap: tenant, lease, stack and attempt are required")
	}
	rawToken, tokenHash, err := pairingtoken.Derive(i.seed, tenantID, "guard-reenroll:"+leaseID+":"+attemptID)
	if err != nil {
		return "", fmt.Errorf("guardbootstrap: derive re-enrollment capability")
	}
	metadata, err := json.Marshal(map[string]string{
		"lease_id":            leaseID,
		"reenroll_attempt_id": attemptID,
		"reenroll_server_id":  strings.TrimSpace(request.ServerID),
		"source":              ReenrollCapabilitySource,
	})
	if err != nil {
		return "", fmt.Errorf("guardbootstrap: encode capability metadata: %w", err)
	}
	tx, err := tenantcontext.Database(ctx, i.database).BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if err := tenantcontext.ApplyTenant(ctx, tx, tenantID); err != nil {
		return "", err
	}
	var owner string
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(owner_subject_id, '') FROM servers WHERE tenant_id = $1 AND stack_id = $2 AND lease_id = $3 ORDER BY id LIMIT 1`,
		tenantID, stackID, leaseID,
	).Scan(&owner); err != nil || strings.TrimSpace(owner) == "" {
		return "", ErrStackOwnerUnknown
	}
	// Insert-once, as RecordCapability: a replay of the same attempt must not
	// re-open a capability the node already spent.
	if _, err := tx.ExecContext(ctx, `
INSERT INTO pairing_tokens (
    id, tenant_id, stack_id, owner_subject_id, name, token_hash,
    status, expires_at, metadata_json
) VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8::jsonb)
ON CONFLICT (tenant_id, token_hash) DO NOTHING`,
		capabilityID(tokenHash), tenantID, stackID, strings.TrimSpace(owner),
		"managed-guard-reenroll "+attemptID, tokenHash, i.now().UTC().Add(reenrollWindow), string(metadata),
	); err != nil {
		return "", fmt.Errorf("guardbootstrap: record re-enrollment capability: %w", err)
	}
	var status string
	var expires time.Time
	if err := tx.QueryRowContext(ctx,
		`SELECT status, expires_at FROM pairing_tokens WHERE tenant_id = $1 AND token_hash = $2`,
		tenantID, tokenHash,
	).Scan(&status, &expires); err != nil {
		return "", fmt.Errorf("guardbootstrap: read back re-enrollment capability: %w", err)
	}
	if status != "active" || !expires.After(i.now().UTC().Add(reenrollMinRemaining)) {
		return "", ErrReenrollCapabilitySpent
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return rawToken, nil
}
