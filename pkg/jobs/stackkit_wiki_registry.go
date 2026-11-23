package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

	"github.com/kombifyio/homelab/internal/stackkitrelease"
)

const wikiRegistrySecretRef = "secret://stackkits/wiki/registry-auth"

type WikiRegistryIssuer func(ctx context.Context, tenantID, ownerID, stackID, workerID string) ([]byte, error)
type WikiRegistryRenewal func(context.Context, StackKitLifecycleRequest) (string, error)

func renewWikiRegistryPull(ctx context.Context, cfg StackKitLifecycleConfig, jobID string, req StackKitLifecycleRequest, release stackkitrelease.Release, outcome map[string]interface{}) error {
	if req.Offline || req.DryRun {
		return nil
	}
	raw := req.CandidateSpecJSON
	if len(raw) == 0 && cfg.WikiRegistrySpec != nil {
		var err error
		raw, err = cfg.WikiRegistrySpec(ctx, req)
		if err != nil {
			return err
		}
	}
	if len(raw) == 0 {
		if req.UseCase == "wiki" {
			return errors.New("Wiki registry needs the deployment's approved StackSpec")
		}
		return nil
	}
	nodeRef := req.NodeRef
	if nodeRef == "" {
		nodeRef = PrimaryStackKitNodeRef(req.StackKit)
	}
	needed, err := wikiRegistrySelected(raw, nodeRef)
	if err != nil {
		return err
	}
	if !needed {
		if req.UseCase == "wiki" {
			return errors.New("Wiki is not selected on this server")
		}
		return nil
	}
	if cfg.WikiRegistryRenew == nil || cfg.WalletHostDelivery == nil {
		return errors.New("Wiki registry renewal requires the connected server and Wallet host delivery")
	}
	itemID, err := cfg.WikiRegistryRenew(ctx, req)
	if err != nil {
		return err
	}
	outcome["wallet_item_id"] = itemID
	req.WalletItemID, req.wikiRegistryItem, req.WalletItemRotate = itemID, itemID, false
	_, _, err = deliverWalletItemToNode(ctx, cfg, jobID+"-wiki-registry", req, release, outcome, func(string, int, string) {})
	return err
}

func wikiRegistrySelected(raw []byte, nodeRef string) (bool, error) {
	var spec struct {
		Workloads map[string]json.RawMessage `json:"workloads"`
		Nodes     []struct {
			ID      string `json:"id"`
			SiteRef string `json:"siteRef"`
			Enabled *bool  `json:"enabled"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		return false, errors.New("Wiki registry could not read the approved workload selection")
	}
	if len(spec.Workloads["wiki"]) == 0 {
		return false, nil
	}
	var wiki struct {
		Enabled   *bool `json:"enabled"`
		Placement struct {
			NodeRefs []string `json:"nodeRefs"`
			SiteRefs []string `json:"siteRefs"`
		} `json:"placement"`
		SecretRefs map[string]string `json:"secretRefs"`
	}
	if err := json.Unmarshal(spec.Workloads["wiki"], &wiki); err != nil {
		return false, errors.New("Wiki registry could not read the selected workload placement")
	}
	if wiki.Enabled != nil && !*wiki.Enabled {
		return false, nil
	}
	if len(wiki.Placement.NodeRefs) == 0 {
		for _, node := range spec.Nodes {
			if (node.Enabled == nil || *node.Enabled) && (len(wiki.Placement.SiteRefs) == 0 || slices.Contains(wiki.Placement.SiteRefs, node.SiteRef)) {
				wiki.Placement.NodeRefs = append(wiki.Placement.NodeRefs, node.ID)
			}
		}
		if len(wiki.Placement.NodeRefs) != 1 {
			return false, errors.New("Wiki registry requires an explicit workload node placement when its site does not resolve to one node")
		}
	}
	if !slices.Contains(wiki.Placement.NodeRefs, nodeRef) {
		return false, nil
	}
	if wiki.SecretRefs["registry-auth"] != wikiRegistrySecretRef {
		return false, errors.New("Wiki registry-auth must use its fixed encrypted node custody slot")
	}
	return true, nil
}
