package routes

import (
	"context"

	"github.com/kombifyio/homelab/pkg/api/agentpb"
	"github.com/kombifyio/homelab/pkg/jobs"
)

func (h *serviceRuntimeHandlers) adoptionReleaseForStack(ctx context.Context, tenant, stack string) (*agentpb.StackKitReleasePin, error) {
	if jobs.StageCandidateReleaseConfigured() {
		return jobs.ApplicationAdoptionReleaseForStack(ctx, tenant, stack, false)
	}
	return h.applicationAdmission()
}
