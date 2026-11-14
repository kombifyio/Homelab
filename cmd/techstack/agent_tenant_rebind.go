package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/kombifyio/techstack/pkg/agent/hostidentity"
	"github.com/kombifyio/techstack/pkg/agent/journal"
	"github.com/kombifyio/techstack/pkg/guardbinding"
	"github.com/kombifyio/techstack/pkg/guardexec"
	"github.com/kombifyio/techstack/pkg/guardidentity"
)

// Same-subject tenant rebind on the Guard (platform-ilxzn). The heartbeat
// stores an operator's offer; a refusal of the legacy credential marks it
// ready; the Guard stops; the restart applies it here before anything reads
// the enrollment or opens the journal.

// applyReadyGuardTenantRebind applies a ready offer: enrollment first, then
// the journal. It returns an error only when an offer is ready but could not
// be applied; the Guard then stays on its legacy binding and retries on the
// next start.
func applyReadyGuardTenantRebind(manager *hostidentity.Manager, custody *os.Root, enrollmentFile string, log *slog.Logger) error {
	receipt, err := manager.ApplyReadyTenantRebind(enrollmentFile, func(from, to string) (int, error) {
		store, openErr := journal.OpenRoot(custody, journal.Options{})
		if openErr != nil {
			return 0, openErr
		}
		defer store.Close()
		if denied := store.Denied(); denied != nil {
			return 0, denied
		}
		return store.RelabelTenant(from, to)
	})
	if err != nil {
		return fmt.Errorf("apply the stored tenant rebind: %w", err)
	}
	if receipt != nil {
		log.Info("guard_tenant_rebind_applied", "directive_id", receipt.DirectiveID,
			"from_tenant", receipt.FromTenantID, "to_tenant", receipt.ToTenantID, "journal_entries", receipt.JournalEntries)
	}
	return nil
}

// watchGuardTenantRebind stops execution once a stored offer became ready in
// this process.
func watchGuardTenantRebind(ctx context.Context, manager *hostidentity.Manager, stop context.CancelFunc) {
	select {
	case <-ctx.Done():
	case <-manager.TenantRebindReadyNotify():
		stop()
	}
}

// guardTenantRebindCustody binds the Manager's rebind custody to this Guard's
// current tenant and worker for the HTTPS heartbeat.
type guardTenantRebindCustody struct {
	manager        *hostidentity.Manager
	identity       *hostidentity.Client
	tenantID       string
	runtimeAgentID string
}

func (c guardTenantRebindCustody) StageTenantRebind(offer guardidentity.TenantRebindOffer) error {
	return c.manager.StageTenantRebind(offer, c.tenantID, c.runtimeAgentID)
}

func (c guardTenantRebindCustody) StagedTenantRebindID() string {
	return c.manager.StagedTenantRebindID()
}

// TenantRebindReadyAfterRefusal proves the stored offer through the identity
// client; a Guard without one cannot prove it and is never rebound remotely.
func (c guardTenantRebindCustody) TenantRebindReadyAfterRefusal(ctx context.Context) bool {
	return c.identity != nil && c.identity.ProbeTenantRebind(ctx)
}

// guardTenantRebindRestart reports whether err (or a ready offer) means the
// Guard stops to apply its tenant rebind on restart.
func guardTenantRebindRestart(manager *hostidentity.Manager, err error) bool {
	return errors.Is(err, hostidentity.ErrTenantRebindReady) || (manager != nil && manager.TenantRebindReady())
}

// rotateExecutorBindingPins hands a pending signed binding key rotation of an
// applied tenant rebind to the privileged executor, which verifies it against
// its own pins. A failure leaves its pins unchanged (mutations stay refused,
// observations continue) and is retried on the next start.
func rotateExecutorBindingPins(ctx context.Context, execution guardExecution, manager *hostidentity.Manager, log *slog.Logger) {
	receipt, pending := manager.PendingPinRotation()
	if !pending || execution.remote == nil {
		return
	}
	var rotation guardbinding.Binding
	if err := json.Unmarshal(receipt.BindingPinRotation, &rotation); err != nil {
		log.Error("guard_binding_pin_rotation_unreadable", "directive_id", receipt.DirectiveID)
		return
	}
	keys := make([]guardbinding.PinnedKey, 0, len(receipt.BindingSigningKeys))
	for _, key := range receipt.BindingSigningKeys {
		keys = append(keys, guardbinding.PinnedKey{KeyID: key.KeyID, PublicKey: key.PublicKey})
	}
	if err := execution.remote.RotateBindingPins(ctx, guardexec.BindingPinsRotateParams{Rotation: rotation, Keys: keys}); err != nil {
		log.Error("guard_binding_pin_rotation_refused", "directive_id", receipt.DirectiveID, "error", err.Error(),
			"guidance", "the executor keeps its pins and refuses mutating commands; the rotation is retried on the next start")
		return
	}
	if err := manager.MarkPinsRotated(receipt.DirectiveID); err != nil {
		log.Warn("guard_binding_pin_rotation_not_recorded", "directive_id", receipt.DirectiveID, "error", err.Error())
		return
	}
	log.Info("guard_binding_pins_rotated", "directive_id", receipt.DirectiveID, "keys", len(keys))
}
