package main

import (
	"context"

	"github.com/kombifyio/homelab/internal/federationfabric"
	"github.com/kombifyio/homelab/pkg/jobs"
)

// federationFabricAdapter lets the managed Modern rollout ask Homelab's
// fabric authority for the external inter-site link binding.
type federationFabricAdapter struct{ issuer *federationfabric.Issuer }

func (adapter federationFabricAdapter) IssueBinding(_ context.Context, request jobs.FederationBindingRequest) ([]byte, error) {
	return adapter.issuer.Issue(federationfabric.Request{
		TenantID: request.TenantID, StackID: request.StackID, Requirement: request.Requirement,
		StackKitsVersion: request.StackKitsVersion, CandidateDigest: request.CandidateDigest,
	})
}

func federationFabric() jobs.FederationFabric {
	return federationFabricAdapter{issuer: federationfabric.NewIssuer()}
}
