package executioncustody

import (
	"context"
	"errors"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/db"
	"github.com/kombifyio/techstack/pkg/guardbinding"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
	"google.golang.org/protobuf/encoding/protojson"
)

// scopeDeployment derives bootstrap scope from the existing product record;
// a configuration never invents the deployment id that Create will allocate.
func (p *Provider) scopeDeployment(ctx context.Context, grant CallerGrant, tenant, deployment, owner string) error {
	if tenant != grant.TenantID || owner == "" || owner != grant.OwnerID || !deploymentIDPattern.MatchString(deployment) {
		return errors.New("execution deployment scope denied")
	}
	if grant.DeploymentAdmission != "owned-records" && !grant.admits(tenant, deployment, owner) {
		return errors.New("execution deployment grant denied")
	}
	tx, err := db.BeginTenantTransaction(ctx, p.AuditDatabase, tenant)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var recordedOwner string
	if err := tx.QueryRowContext(ctx, `SELECT owner_subject_id FROM stacks WHERE tenant_id=$1 AND id=$2`, tenant, deployment).Scan(&recordedOwner); err != nil || recordedOwner != owner {
		return errors.New("execution deployment owner is not recorded")
	}
	return nil
}

// verifyTrustImport reuses the canonical Guard result admission. The caller
// cannot choose the first local Owner: the completed bound command of this
// deployment's actual worker must already contain that exact import receipt.
func (p *Provider) verifyTrustImport(ctx context.Context, grant CallerGrant, binding TrustBinding) error {
	if p.scopeDeployment(ctx, grant, binding.TenantID, binding.DeploymentID, grant.OwnerID) != nil {
		return errors.New("execution trust deployment denied")
	}
	tx, err := db.BeginTenantTransaction(ctx, p.AuditDatabase, binding.TenantID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT c.command_json,c.result_json,c.agent_id,c.result_generation FROM typed_agent_commands c
 JOIN servers s ON s.tenant_id=c.tenant_id AND s.worker_id=c.agent_id
 WHERE c.tenant_id=$1 AND s.stack_id=$2 AND s.owner_subject_id=$3 AND c.state='completed'
 AND c.command_family='stackkit' AND c.contract_version=$4 AND c.result_generation>=0
 AND c.result_generation=c.worker_generation AND c.result_revision>0 AND c.result_digest<>''
 ORDER BY c.completed_at DESC LIMIT 64`, binding.TenantID, binding.DeploymentID, grant.OwnerID, guardbinding.ContractVersion)
	if err != nil {
		return err
	}
	type receipt struct {
		command, result []byte
		agent           string
		generation      int64
	}
	var receipts []receipt
	for rows.Next() {
		var r receipt
		if err := rows.Scan(&r.command, &r.result, &r.agent, &r.generation); err != nil {
			_ = rows.Close()
			return err
		}
		receipts = append(receipts, r)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	_ = tx.Rollback()
	// Generation 0 is a key-bound Guard that was never rotated. Its result
	// counts only while that worker still reads generation 0 and its bound host
	// key is its last proven identity (controlplane.CommandAuthorityGeneration,
	// kombify-StackKits-4czp). The identity is read only for such a receipt, so
	// generation >= 1 evidence keeps its existing reads.
	keyBound := map[string]bool{}
	store := controlplane.NewPostgresStore(p.AuditDatabase)
	for _, r := range receipts {
		if r.generation == 0 {
			admitted, seen := keyBound[r.agent]
			if !seen {
				worker, err := store.GetWorker(ctx, binding.TenantID, r.agent)
				if err == nil && worker != nil {
					generation, ok := controlplane.CommandAuthorityGeneration(*worker, func() (*controlplane.WorkerIdentity, error) {
						return store.GetWorkerIdentity(ctx, binding.TenantID, r.agent)
					})
					admitted = ok && generation == 0
				}
				keyBound[r.agent] = admitted
			}
			if !admitted {
				continue
			}
		}
		command, result := &agentpb.StackKitCommand{}, &agentpb.StackKitResult{}
		if protojson.Unmarshal(r.command, command) != nil || protojson.Unmarshal(r.result, result) != nil || command.GetStackkitInstanceId() != binding.StackID {
			continue
		}
		if stackkitcommand.ValidateResult(result, command) != nil {
			continue
		}
		evidence, err := stackkitcommand.ParseAdvancedTrustImportEvidence(command, result)
		if err == nil && evidence.OwnerRef == binding.OwnerRef && evidence.BundleSHA256 == binding.BundleSHA256 {
			return nil
		}
	}
	return errors.New("authenticated Guard trust-import receipt is not recorded")
}
