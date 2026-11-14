package household

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/orchestrator"
)

// A removal ends the person's access on every server of the homelab that holds
// their identity, not only on the server their projection binding names
// (HOMELAB-IDENTITY-ACCESS-TARGET.md section 10, platform-8o1zz.3). The bound
// server (the Authority) keeps its projection key. Every other server gets its
// own revisioned HOUSEHOLD_PROJECT removal under a key bound to that server and
// its enrolled runtime, so one server's unresolved outcome (the Guard's
// reconciler settles a projection as not applied, platform-cztj4.2) never
// fences another server and a re-enrolled node starts a fresh attempt. Each
// command runs the node's one removal chain, which is idempotent: a server
// that holds nothing for the person answers removed.

// serverRemovalKey is the projection key of the removal on one non-bound server.
func serverRemovalKey(owner, subject string, target Target) string {
	sum := sha256.Sum256([]byte("household-server-removal\n" + owner + "\n" + subject + "\n" +
		target.StackID + "\n" + target.AgentID + "\n" + target.StackKitInstanceID))
	return hex.EncodeToString(sum[:])
}

// removeOnOtherServers sends the revisioned removal to every other server of the
// owner's homelab that can hold the person: each identity stack, and each stack
// a saved reference names. A server that has no reference and answers
// not-found or lifecycle-unavailable (no enrolled runtime) is skipped, because
// nothing was ever projected to it; a server that holds a reference, and any
// other resolve error, must be answered, so the removal stays pending.
// Servers are attempted independently and in order; every failure is joined.
// The D10 reference of a server is deleted only after its removal command
// completed. It returns the removed server ids.
func (s *Service) removeOnOtherServers(ctx context.Context, tenantID, owner string, binding controlplane.HouseholdProjectionBinding, revision uint64) ([]string, error) {
	runtime, ok := s.Identity.(RevisionedIdentityRuntime)
	if !ok {
		return nil, ErrNodeUnreachable
	}
	resolver, ok := s.Identity.(ProjectionTargetResolver)
	if !ok {
		return nil, ErrNodeUnreachable
	}
	refs, err := s.Links.ListHouseholdRefsByOwner(ctx, tenantID, owner)
	if err != nil {
		return nil, err
	}
	referenced := map[string]controlplane.HomelabIdentityRef{}
	for _, ref := range refs {
		if ref.CloudSubject == binding.CloudSubject && ref.StackID != binding.StackID {
			referenced[ref.StackID] = ref
		}
	}
	servers := map[string]bool{}
	for id := range referenced {
		servers[id] = true
	}
	homelab, err := s.Homelabs.GetHomelabByOwner(ctx, tenantID, owner)
	switch {
	case errors.Is(err, controlplane.ErrNotFound) || (err == nil && homelab == nil):
	case err != nil:
		return nil, err
	default:
		stacks, err := s.identityStacks(ctx, tenantID, owner, homelab.ID)
		if err != nil {
			return nil, err
		}
		for _, stack := range stacks {
			if stack.ID != binding.StackID {
				servers[stack.ID] = true
			}
		}
	}
	ids := make([]string, 0, len(servers))
	for id := range servers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	removed := []string{}
	var pending error
	for _, id := range ids {
		ref, hasRef := referenced[id]
		target, err := resolver.ResolveProjectionTarget(ctx, Target{TenantID: tenantID, OwnerID: owner, StackID: id})
		if err == nil && (target.TenantID != tenantID || target.OwnerID != owner || target.StackID != id || target.AgentID == "" || target.StackKitInstanceID == "") {
			err = controlplane.ErrHouseholdProjectionConflict
		}
		if err != nil {
			// Only "never enrolled" lets a server without a saved reference be
			// skipped. Any other error, including a transient read failure,
			// keeps the removal pending so Cloud re-sends it and the D10 link
			// does not end.
			if !hasRef && (errors.Is(err, controlplane.ErrNotFound) || errors.Is(err, orchestrator.ErrStackKitLifecycleUnavailable)) {
				continue
			}
			pending = errors.Join(pending, fmt.Errorf("%w: server %s: %v", ErrNodeUnreachable, id, err))
			continue
		}
		// The account a reference names is the one that exists on that server.
		username := binding.Username
		if hasRef && ref.Username != "" {
			username = ref.Username
		}
		if _, err := runtime.Project(ctx, target, IdentityProjection{
			Key: serverRemovalKey(owner, binding.CloudSubject, target), Revision: revision, Action: ActionRemove, Username: username,
		}); err != nil {
			pending = errors.Join(pending, fmt.Errorf("%w: server %s: %v", ErrNodeUnreachable, id, err))
			continue
		}
		if hasRef {
			if _, err := s.Links.DeleteHouseholdRef(ctx, tenantID, id, binding.CloudSubject); err != nil && !errors.Is(err, controlplane.ErrNotFound) {
				pending = errors.Join(pending, err)
				continue
			}
		}
		removed = append(removed, id)
	}
	return removed, pending
}
