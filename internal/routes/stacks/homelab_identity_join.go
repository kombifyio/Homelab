package stacks

import (
	"errors"
	"net/http"
	"strings"

	"github.com/kombifyio/techstack/internal/routes/tenantguard"
	ksapi "github.com/kombifyio/techstack/pkg/api"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/orchestrator"
)

// startHomelabIdentityJoin joins one kit deployment of the caller's homelab
// to the homelab's identity Authority (homelab identity target §4, ADR-036).
// The Authority is the homelab's authority pointer, never a client choice;
// the Owner enters the Authority's root fingerprint as the Authority's
// console or Hub shows it, and approves the join and the sign-in client with
// passkey step-ups in the Authority's Hub while the job waits.
func (h crudRouteHandlers) startHomelabIdentityJoin(e *httpx.Event) error {
	owner, err := requireStackAuth(e)
	if err != nil {
		return err
	}
	tenant, err := tenantguard.TenantScope(tenantIDFromRequest(e), owner, "techstack.homelab.identity-join")
	if err != nil {
		return err
	}
	if h.orch == nil || h.homelabStore == nil || h.stackStore == nil {
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Homelab identity join is unavailable", map[string]any{
			detailsKeyReasonCode: "homelab_authority_unavailable", detailsKeyRetryable: true,
		})
	}
	var body struct {
		AuthorityRoot string   `json:"authority_root"`
		Hostnames     []string `json:"hostnames,omitempty"`
		OwnerApproved bool     `json:"owner_approved"`
	}
	if err := decodeStrictJSONBody(e.Request.Body, &body); err != nil {
		return httpx.BadRequest(e, "Invalid identity join request")
	}
	if !body.OwnerApproved || strings.TrimSpace(body.AuthorityRoot) == "" {
		return httpx.BadRequest(e, "The Authority's root fingerprint and explicit Owner approval are required")
	}
	if len(e.Request.Header.Values("Idempotency-Key")) != 1 {
		return httpx.BadRequest(e, "Exactly one Idempotency-Key is required")
	}
	ctx := e.Request.Context()
	homelab, err := h.homelabStore.GetHomelabByOwner(ctx, tenant, owner)
	if errors.Is(err, controlplane.ErrNotFound) {
		return httpx.NotFound(e, "No homelab provisioned yet")
	}
	if err != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to resolve homelab", nil)
	}
	pointer := homelab.IdentityAuthority
	if pointer == nil || pointer.StackID == "" || pointer.ServerID == "" || pointer.Departed {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The homelab has no current identity Authority", map[string]any{
			detailsKeyReasonCode: "homelab_authority_missing", detailsKeyRetryable: false,
		})
	}
	joiningID := strings.TrimSpace(e.Request.PathValue("id"))
	joining, err := h.stackStore.GetStack(ctx, tenant, joiningID)
	if err != nil || joining == nil || joining.OwnerSubjectID != owner || joining.HomelabID != homelab.ID {
		return httpx.NotFound(e, "Kit deployment not found in this homelab")
	}
	if joining.ID == pointer.StackID {
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "The homelab's Authority does not join itself", nil)
	}
	stacks, err := h.stackStore.ListStacksByTenant(ctx, tenant)
	if err != nil {
		return httpx.Error(e, http.StatusInternalServerError, ksapi.ErrCodeInternal, "Failed to resolve homelab", nil)
	}
	others := []string{}
	for _, stack := range stacks {
		if stack.OwnerSubjectID == owner && stack.HomelabID == homelab.ID && stack.ID != pointer.StackID && stack.ID != joining.ID {
			others = append(others, stack.ID)
		}
	}
	input := orchestrator.HomelabIdentityJoinInput{
		HomelabID: homelab.ID, AuthorityStackID: pointer.StackID, AuthorityServerID: pointer.ServerID, JoiningStackID: joining.ID,
		OtherStackIDs: others, AuthorityRoot: body.AuthorityRoot, Hostnames: body.Hostnames, OwnerApproved: body.OwnerApproved,
	}
	id, err := h.orch.EnqueueHomelabIdentityJoin(ctx, tenant, owner, e.Request.Header.Get("Idempotency-Key"), input)
	switch {
	case errors.Is(err, controlplane.ErrNotFound):
		return httpx.NotFound(e, "Kit deployment not found in this homelab")
	case errors.Is(err, controlplane.ErrConflict):
		return httpx.Error(e, http.StatusConflict, ksapi.ErrCodeConflict, "Identity join conflicts with an existing operation", nil)
	case err != nil:
		return httpx.Error(e, http.StatusServiceUnavailable, ksapi.ErrCodeUnavailable, "Identity join prerequisites are unavailable", map[string]any{"reason": err.Error()})
	}
	return httpx.Success(e, http.StatusAccepted, map[string]any{
		"job_id": id, "kit_deployment_id": joining.ID, "authority_kit_deployment_id": pointer.StackID, "operation": "homelab_identity_join",
	})
}
