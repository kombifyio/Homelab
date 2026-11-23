package orchestrator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/kombifywallet"
)

func (o *Orchestrator) ConfigureWikiRegistryIssuer(issuer jobs.WikiRegistryIssuer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg.WikiRegistryIssuer = issuer
}

func (o *Orchestrator) wikiRegistrySpec(ctx context.Context, req jobs.StackKitLifecycleRequest) ([]byte, error) {
	stack, err := o.findStackForJob(ctx, req.StackID, ProvisionStackOptions{RequestContext: ctx, TenantID: req.TenantID, OwnerID: req.OwnerID})
	if err != nil {
		return nil, err
	}
	if stack.ownerID != req.OwnerID || stack.tenantID != req.TenantID || stack.stackKitInstanceID != req.StackKitInstanceID {
		return nil, ErrStackKitLifecycleUnavailable
	}
	if spec, ok := stack.config["stack_spec_v2"].(map[string]any); ok {
		return json.Marshal(spec)
	}
	return nil, nil
}

func wikiRegistryRenewal(issuer jobs.WikiRegistryIssuer, wallet *kombifywallet.Client) jobs.WikiRegistryRenewal {
	if issuer == nil || !wallet.Enabled() {
		return nil
	}
	return func(ctx context.Context, req jobs.StackKitLifecycleRequest) (string, error) {
		scope, ok := kombifywallet.ProjectScope(req.StackID)
		if !ok {
			return "", errors.New("Wiki registry renewal requires the deployment's Wallet scope")
		}
		grant, err := issuer(ctx, req.TenantID, req.OwnerID, req.StackID, req.AgentID)
		if err != nil {
			return "", err
		}
		defer clear(grant)
		workerDigest := sha256.Sum256([]byte(req.AgentID))
		item, err := wallet.Ensure(ctx, req.OwnerID, kombifywallet.ItemSpec{
			Type: kombifywallet.ItemTypeGenericSecret, Scope: scope,
			DisplayName: "Wiki image pull " + hex.EncodeToString(workerDigest[:8]),
			Description: "Short-lived pull grant for this deployment's connected Wiki server. Allow host delivery only to that server.",
			Value:       grant,
		})
		if err != nil {
			return "", err
		}
		return item.ItemID, nil
	}
}
