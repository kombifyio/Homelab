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

func (o *Orchestrator) ConfigureCustomerRegistryIssuer(issuer jobs.CustomerRegistryIssuer) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg.CustomerRegistryIssuer = issuer
}

func (o *Orchestrator) customerRegistrySpec(ctx context.Context, req jobs.StackKitLifecycleRequest) ([]byte, error) {
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

func customerRegistryRenewal(issuer jobs.CustomerRegistryIssuer, wallet *kombifywallet.Client) jobs.CustomerRegistryRenewal {
	if issuer == nil || !wallet.Enabled() {
		return nil
	}
	return func(ctx context.Context, req jobs.StackKitLifecycleRequest, product string) (string, error) {
		scope, ok := kombifywallet.ProjectScope(req.StackID)
		if !ok {
			return "", errors.New("customer registry renewal requires the deployment's Wallet scope")
		}
		grant, err := issuer(ctx, req.TenantID, req.OwnerID, req.StackID, req.AgentID, product)
		if err != nil {
			return "", err
		}
		defer clear(grant)
		workerDigest := sha256.Sum256([]byte(req.AgentID))
		item, err := wallet.Ensure(ctx, req.OwnerID, kombifywallet.ItemSpec{
			Type: kombifywallet.ItemTypeGenericSecret, Scope: scope,
			DisplayName: registryGrantDisplayName(product) + " image pull " + hex.EncodeToString(workerDigest[:8]),
			Description: "Short-lived registry pull grant for this deployment's connected server. Allow host delivery only to that server.",
			Value:       grant,
		})
		if err != nil {
			return "", err
		}
		return item.ItemID, nil
	}
}

func registryGrantDisplayName(product string) string {
	if product == "paperwork" {
		return "Paperwork"
	}
	return "Wiki"
}
