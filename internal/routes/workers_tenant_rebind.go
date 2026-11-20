package routes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/guardbinding"
	"github.com/kombifyio/homelab/pkg/guardidentity"
	"github.com/kombifyio/homelab/pkg/workerauth"
)

// GuardTenantRebindCredential is the credential a same-subject tenant rebind
// hands the Guard and the worker credential state the operator commits under
// the canonical tenant once the rows moved there.
type GuardTenantRebindCredential struct {
	Token    string
	Expected controlplane.WorkerCredentialState
	Next     controlplane.WorkerCredentialState
	// Committed reports that the worker already holds Next: an earlier commit
	// pinned it, and only the directive removal may be left.
	Committed bool
}

// PrepareGuardTenantRebindCredential derives the opaque runtime-agent
// credential of directive for the canonical tenant. It is deterministic, so
// the heartbeat offers the same credential on every beat and the operator
// commit pins exactly its hash without any credential being stored. worker is
// the row the directive was staged on (legacy tenant) or the same row after the
// move (canonical tenant): its credential must still be the generation the
// directive recorded, or already the one this directive derives.
func PrepareGuardTenantRebindCredential(secret []byte, directive guardidentity.TenantRebindDirective, worker controlplane.Worker) (GuardTenantRebindCredential, error) {
	var out GuardTenantRebindCredential
	if len(secret) == 0 {
		return out, workerauth.ErrMissingSecret
	}
	if err := directive.ValidateShape(); err != nil {
		return out, err
	}
	if worker.ID != directive.RuntimeAgentID || strings.TrimSpace(worker.OwnerSubjectID) != directive.SubjectID {
		return out, fmt.Errorf("%w: the worker is not the directive's worker of the same subject", guardidentity.ErrTenantRebindRefused)
	}
	state, err := controlplane.WorkerCredentialStateFromWorker(worker)
	if err != nil {
		return out, err
	}
	next := directive.CredentialGeneration + 1
	token, err := deriveWorkerCredential(secret, workerCredentialRequest{
		TenantID: directive.ToTenantID, OwnerID: directive.SubjectID, StackID: directive.StackID,
		ServerID: directive.ServerID, RuntimeAgentID: directive.RuntimeAgentID,
		RequestDigest: directive.Digest(), IdempotencyKey: directive.IdempotencyKey(),
	}, next)
	if err != nil {
		return out, err
	}
	idempotencyDigest, err := workerauth.KeyedDigest(secret, workerCredentialIdempotencyDigestDomain, directive.IdempotencyKey())
	if err != nil {
		return out, err
	}
	out = GuardTenantRebindCredential{
		Token: token,
		Expected: controlplane.WorkerCredentialState{
			TokenSHA256: state.TokenSHA256, IdempotencySHA256: state.IdempotencySHA256,
			RequestSHA256: state.RequestSHA256, Generation: directive.CredentialGeneration,
		},
		Next: controlplane.WorkerCredentialState{
			TokenSHA256: workerauth.SHA256Hex(token), IdempotencySHA256: idempotencyDigest,
			RequestSHA256: directive.Digest(), Generation: next,
		},
	}
	switch {
	case state.Generation == directive.CredentialGeneration:
		return out, nil
	case state == out.Next:
		out.Committed = true
		return out, nil
	default:
		return GuardTenantRebindCredential{}, fmt.Errorf("%w: the worker credential changed since the directive was staged", guardidentity.ErrTenantRebindRefused)
	}
}

// guardTenantRebindOffer returns the offer for the authenticated Guard when
// the operator staged one for exactly its current tenant, and records the
// Guard's acknowledgement. Every failure only omits the offer: the directive is
// an operator move, never a reason to refuse liveness.
func (h workerRouteHandlers) guardTenantRebindOffer(ctx context.Context, authCtx runtimeAgentAuthContext, acknowledged string, now time.Time) *guardidentity.TenantRebindOffer {
	if authCtx.Worker == nil {
		return nil
	}
	directive, ok := guardidentity.DirectiveFromResource(authCtx.Worker.Resources[controlplane.WorkerTenantRebindResourceKey])
	if !ok || directive.Validate(now) != nil || directive.FromTenantID != authCtx.TenantID ||
		directive.RuntimeAgentID != authCtx.RuntimeAgentID {
		return nil
	}
	credential, err := PrepareGuardTenantRebindCredential(h.configuredWorkerCredentialSecret(), directive, *authCtx.Worker)
	if err != nil || credential.Committed {
		return nil
	}
	offer := &guardidentity.TenantRebindOffer{Directive: directive, AgentToken: credential.Token}
	// Binding keys are derived per tenant and worker: the Guard pins the
	// canonical tenant's keys with the credential, or it would refuse every
	// binding signed after the move.
	signer, signerErr := h.bindingSignerFor(ctx, directive.ToTenantID, directive.RuntimeAgentID)
	if signerErr != nil {
		return nil
	}
	if signer != nil {
		keys := signer.PublicKeys()
		for _, key := range keys {
			offer.BindingSigningKeys = append(offer.BindingSigningKeys, guardidentity.TenantRebindPinnedKey{KeyID: key.KeyID, PublicKey: key.PublicKey})
		}
		rotation, rotationErr := h.guardTenantRebindPinRotation(ctx, directive, keys)
		if rotationErr != nil {
			return nil
		}
		offer.BindingPinRotation = rotation
	}
	if strings.TrimSpace(acknowledged) == directive.DirectiveID && directive.StagedAt == nil {
		if store, ok := h.wst.(controlplane.WorkerTenantRebindStore); ok {
			if _, recordErr := store.RecordWorkerTenantRebindStaged(ctx, authCtx.TenantID, authCtx.RuntimeAgentID, directive.DirectiveID, now); recordErr != nil {
				return nil
			}
		}
	}
	return offer
}

// guardTenantRebindPinRotation signs the canonical tenant's binding keys with
// the worker's legacy-tenant binding key, so an executor that pins the legacy
// keys itself can verify and adopt them (guardbinding.VerifyPinRotation).
// Without a legacy signer nothing was pinned and no rotation is needed.
func (h workerRouteHandlers) guardTenantRebindPinRotation(ctx context.Context, directive guardidentity.TenantRebindDirective, keys []guardbinding.PinnedKey) (json.RawMessage, error) {
	legacy, err := h.bindingSignerFor(ctx, directive.FromTenantID, directive.RuntimeAgentID)
	if err != nil || legacy == nil || len(keys) == 0 {
		return nil, err
	}
	digest, err := guardbinding.PinSetDigest(keys)
	if err != nil {
		return nil, err
	}
	rotation := guardbinding.Binding{
		CommandID: directive.DirectiveID, TenantID: directive.FromTenantID, WorkerID: directive.RuntimeAgentID,
		ServerID: directive.ServerID, Operation: guardbinding.PinRotationOperation, PayloadDigest: digest,
		AuthorityExpiresAt: directive.NotAfter.UTC(), ContractVersion: guardbinding.ContractVersion,
	}
	if err := legacy.Sign(&rotation); err != nil {
		return nil, err
	}
	return json.Marshal(rotation)
}
