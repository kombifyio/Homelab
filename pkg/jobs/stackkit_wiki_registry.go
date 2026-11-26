package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

type CustomerRegistryIssuer func(ctx context.Context, tenantID, ownerID, stackID, workerID, product string) ([]byte, error)
type CustomerRegistryRenewal func(context.Context, StackKitLifecycleRequest, string) (string, error)

func copyRegistryOutcomes(destination, source map[string]interface{}) {
	for _, key := range []string{"wiki_registry", "paperwork_registry"} {
		if outcome, present := source[key]; present {
			destination[key] = outcome
		}
	}
}

type registryWorkload struct {
	Enabled   *bool `json:"enabled"`
	Placement struct {
		NodeRefs []string `json:"nodeRefs"`
		SiteRefs []string `json:"siteRefs"`
	} `json:"placement"`
	SecretRefs map[string]string `json:"secretRefs"`
}

type registrySelectionNode struct {
	ID      string `json:"id"`
	SiteRef string `json:"siteRef"`
	Enabled *bool  `json:"enabled"`
}

type registrySelectionSpec struct {
	Workloads map[string]json.RawMessage `json:"workloads"`
	Nodes     []registrySelectionNode    `json:"nodes"`
}

func renewCustomerRegistryPull(ctx context.Context, cfg StackKitLifecycleConfig, jobID string, req StackKitLifecycleRequest, release stackkitrelease.Release, outcome map[string]interface{}) error {
	if req.DryRun {
		return nil
	}
	if req.Offline && req.Operation == StackKitLifecycleApplicationUpdate && req.UseCase != ManagedWikiWorkload {
		return nil
	}
	if err := ValidateManagedWikiExecution(ctx, cfg, req); err != nil {
		return err
	}
	raw := req.CandidateSpecJSON
	if len(raw) == 0 && cfg.CustomerRegistrySpec != nil {
		var err error
		raw, err = cfg.CustomerRegistrySpec(ctx, req)
		if err != nil {
			return err
		}
	}
	if len(raw) == 0 {
		if req.UseCase == ManagedWikiWorkload || (!req.Offline && req.UseCase == "paperwork") {
			return errors.New("customer registry needs the deployment's approved StackSpec")
		}
		return nil
	}
	nodeRef := req.NodeRef
	if nodeRef == "" {
		nodeRef = PrimaryStackKitNodeRef(req.StackKit)
	}
	for _, product := range []string{ManagedWikiWorkload, "paperwork"} {
		if req.Offline && product != ManagedWikiWorkload {
			continue
		}
		if req.Operation == StackKitLifecycleApplicationUpdate && req.UseCase != product {
			continue
		}
		needed, err := customerRegistrySelected(raw, nodeRef, product)
		if err != nil {
			return err
		}
		if !needed {
			if req.UseCase == product {
				return errors.New(product + " is not selected on this server")
			}
			continue
		}
		// The Wiki digest above was checked against committed authority,
		// rather than treating this candidate as its own admission.
		if req.Offline {
			continue
		}
		if cfg.CustomerRegistryRenew == nil || cfg.WalletHostDelivery == nil {
			return errors.New(product + " registry renewal requires the connected server and Wallet host delivery")
		}
		itemID, err := cfg.CustomerRegistryRenew(ctx, req, product)
		if err != nil {
			return err
		}
		productOutcome := map[string]interface{}{"wallet_item_id": itemID}
		outcome[product+"_registry"] = productOutcome
		delivery := req
		delivery.WalletItemID, delivery.registryItem, delivery.registryProduct, delivery.WalletItemRotate = itemID, itemID, product, false
		if _, _, err := deliverWalletItemToNode(ctx, cfg, jobID+"-"+product+"-registry", delivery, release, productOutcome, func(string, int, string) {}); err != nil {
			return err
		}
	}
	return nil
}

func customerRegistrySelected(raw []byte, nodeRef, product string) (bool, error) {
	var spec registrySelectionSpec
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, errors.New("customer registry could not read the approved workload selection")
	}
	if len(spec.Workloads[product]) == 0 {
		return false, nil
	}
	var workload registryWorkload
	if err := json.Unmarshal(spec.Workloads[product], &workload); err != nil {
		return false, errors.New(product + " registry could not read the selected workload placement")
	}
	if workload.Enabled != nil && !*workload.Enabled {
		return false, nil
	}
	if len(workload.Placement.NodeRefs) == 0 {
		for _, node := range spec.Nodes {
			if (node.Enabled == nil || *node.Enabled) && (len(workload.Placement.SiteRefs) == 0 || slices.Contains(workload.Placement.SiteRefs, node.SiteRef)) {
				workload.Placement.NodeRefs = append(workload.Placement.NodeRefs, node.ID)
			}
		}
		if len(workload.Placement.NodeRefs) != 1 {
			return false, errors.New(product + " registry requires an explicit workload node placement when its site does not resolve to one node")
		}
	}
	if !slices.Contains(workload.Placement.NodeRefs, nodeRef) {
		return false, nil
	}
	if workload.SecretRefs["registry-auth"] != "secret://stackkits/"+product+"/registry-auth" {
		return false, errors.New(product + " registry-auth must use its fixed encrypted node custody slot")
	}
	return true, nil
}
