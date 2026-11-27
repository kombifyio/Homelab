package main

import (
	"context"

	"github.com/kombifyio/homelab/internal/managedstackkit"
	"github.com/kombifyio/homelab/internal/routes"
	storageauth "github.com/kombifyio/homelab/pkg/auth"
	"github.com/kombifyio/homelab/pkg/backupstore"
	"github.com/kombifyio/homelab/pkg/jobs"
	"github.com/kombifyio/homelab/pkg/kombifywallet"
	"github.com/kombifyio/stackkits/pkg/hostdelivery"
)

// managedStackKitInventoryAdapter is the control plane's managed rollout
// authority: the channel-only Inventory and, when backup custody is configured,
// the delivery of the backup credentials to the node that owns them.
type managedStackKitInventoryAdapter struct {
	builder  *managedstackkit.RolloutInventory
	delivery *managedstackkit.BackupDelivery
}

func (adapter managedStackKitInventoryAdapter) Build(ctx context.Context, request jobs.ManagedStackKitInventoryRequest) ([]byte, error) {
	nodes := make([]managedstackkit.NodeChannel, 0, len(request.Nodes))
	for _, node := range request.Nodes {
		nodes = append(nodes, managedstackkit.NodeChannel{ChannelRef: node.ChannelRef, SiteRef: node.SiteRef, NodeRef: node.NodeRef})
	}
	return adapter.builder.Build(ctx, managedstackkit.RolloutInventoryRequest{
		TenantID: request.TenantID, StackID: request.StackID, ResolvedPlan: request.ResolvedPlan, Nodes: nodes,
	})
}

func managedStackKitInventoryBuilder(deps routeDeps) jobs.ManagedStackKitInventoryBuilder {
	executableDigest, err := routes.CurrentAgentBinarySHA256()
	if err != nil {
		deps.log.Warn("managed_stackkit_inventory_unavailable", "reason", "operations_binary_unavailable", "error", err)
		return nil
	}
	builder, err := managedstackkit.NewRolloutInventory(executableDigest)
	if err != nil {
		deps.log.Warn("managed_stackkit_inventory_unavailable", "reason", "inventory_builder_invalid", "error", err)
		return nil
	}
	channelOnly := managedStackKitInventoryAdapter{builder: builder}
	if deps.v2 == nil || deps.v2.db == nil || deps.v2.db.DB == nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "database_not_configured")
		return channelOnly
	}
	repository, err := backupstore.NewPostgresCustodyStore(deps.v2.db.DB, storageauth.GetEncryptor())
	if err != nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "encrypted_custody_not_configured", "error", err)
		return channelOnly
	}
	providerConfig, err := backupstore.FromEnv()
	if err != nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "backup_provider_not_configured", "error", err)
		return channelOnly
	}
	provider, err := backupstore.NewClient(providerConfig)
	if err != nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "backup_provider_invalid", "error", err)
		return channelOnly
	}
	custodian, err := backupstore.NewCustodian(provider, repository)
	if err != nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "custodian_not_configured", "error", err)
		return channelOnly
	}
	var options []managedstackkit.BackupDeliveryOption
	if deps.walletSource.IsKombify() {
		// Wallet mode: the Wallet seals and signs every backup delivery; without
		// its client or delivering key every delivery fails closed.
		options = append(options, managedstackkit.WithWallet(managedBackupWallet(deps.wallet, deps.log)))
	}
	delivery, err := managedstackkit.NewBackupDelivery(custodian, repository, options...)
	if err != nil {
		deps.log.Warn("managed_stackkit_backup_inventory_unavailable", "reason", "backup_delivery_invalid", "error", err)
		return channelOnly
	}
	return managedStackKitInventoryAdapter{builder: builder, delivery: delivery}
}

// BackupDelivery exposes delivery only when custody is configured, so a stack
// that selects the off-site capability without it fails closed.
func (adapter managedStackKitInventoryAdapter) BackupDelivery() jobs.ManagedBackupTargetDelivery {
	if adapter.delivery == nil {
		return nil
	}
	return managedBackupDeliveryAdapter{delivery: adapter.delivery}
}

// managedBackupDeliveryAdapter carries the job layer's types to the control
// plane's delivery authority.
type managedBackupDeliveryAdapter struct {
	delivery *managedstackkit.BackupDelivery
}

func (adapter managedBackupDeliveryAdapter) NewChallenge() (string, error) {
	return adapter.delivery.NewChallenge()
}

func (adapter managedBackupDeliveryAdapter) AdmitRecipient(ctx context.Context, tenantID, stackID, challenge string, statement []byte) (jobs.ManagedBackupRecipient, error) {
	recipient, err := adapter.delivery.AdmitRecipient(ctx, managedstackkit.DeliveryScope{TenantID: tenantID, StackID: stackID}, challenge, statement)
	if err != nil {
		return jobs.ManagedBackupRecipient{}, err
	}
	return jobs.ManagedBackupRecipient{
		Public: recipient.Public, Fingerprint: recipient.Fingerprint, Generation: recipient.Generation, SiteRef: recipient.SiteRef, NodeRef: recipient.NodeRef,
		Statement: recipient.Statement, Challenge: recipient.Challenge,
	}, nil
}

func (adapter managedBackupDeliveryAdapter) DeliveryAuthority() (string, string, error) {
	key, keyID, _, err := adapter.delivery.WalletAuthority()
	return key, keyID, err
}

func (adapter managedBackupDeliveryAdapter) Seal(ctx context.Context, tenantID, stackID, ownerID string, recipient jobs.ManagedBackupRecipient, operation string) ([]byte, error) {
	sealOperation := hostdelivery.OperationBackupTargetImport
	if operation == jobs.StackKitLifecycleBackupTargetRebind {
		sealOperation = hostdelivery.OperationBackupTargetRebind
	}
	return adapter.delivery.Seal(ctx, managedstackkit.DeliveryScope{TenantID: tenantID, StackID: stackID, OwnerID: ownerID}, managedstackkit.Recipient{
		Public: recipient.Public, Fingerprint: recipient.Fingerprint, Generation: recipient.Generation, SiteRef: recipient.SiteRef, NodeRef: recipient.NodeRef,
		Statement: recipient.Statement, Challenge: recipient.Challenge, PlacementRef: kombifywallet.PlacementRef(recipient.AgentID),
	}, sealOperation)
}

func (adapter managedBackupDeliveryAdapter) RecordNodeBinding(ctx context.Context, tenantID, stackID string, binding jobs.ManagedBackupNodeBinding) error {
	return adapter.delivery.RecordNodeBinding(ctx, managedstackkit.DeliveryScope{TenantID: tenantID, StackID: stackID}, backupstore.NodeBinding{BackupTargetRef: binding.BackupTargetRef, CustodyAttestationRef: binding.CustodyAttestationRef})
}

func (adapter managedBackupDeliveryAdapter) NodeBinding(ctx context.Context, tenantID, stackID string) (jobs.ManagedBackupNodeBinding, error) {
	binding, err := adapter.delivery.NodeBinding(ctx, managedstackkit.DeliveryScope{TenantID: tenantID, StackID: stackID})
	if err != nil {
		return jobs.ManagedBackupNodeBinding{}, err
	}
	return jobs.ManagedBackupNodeBinding{BackupTargetRef: binding.BackupTargetRef, CustodyAttestationRef: binding.CustodyAttestationRef}, nil
}

func (adapter managedBackupDeliveryAdapter) AttestTarget(ctx context.Context, tenantID, stackID string) (string, error) {
	return adapter.delivery.AttestTarget(ctx, tenantID, stackID)
}

func (adapter managedBackupDeliveryAdapter) NodeOwnerPublicKey(ctx context.Context, tenantID, stackID string) ([]byte, error) {
	return adapter.delivery.NodeOwnerPublicKey(ctx, tenantID, stackID)
}

func (adapter managedBackupDeliveryAdapter) AdmitReplacementRecipient(ctx context.Context, scope jobs.ManagedBackupReplacementScope, challenge string, statement []byte) (jobs.ManagedBackupRecipient, error) {
	recipient, err := adapter.delivery.AdmitReplacementRecipient(ctx, managedstackkit.ReplacementScope{
		TenantID:                      scope.TenantID,
		StackID:                       scope.StackID,
		OwnerID:                       scope.OwnerID,
		ReplacementID:                 scope.ReplacementID,
		DestinationLeaseID:            scope.DestinationLeaseID,
		DestinationServerID:           scope.DestinationServerID,
		DestinationResourceGeneration: scope.DestinationResourceGeneration,
	}, challenge, statement)
	if err != nil {
		return jobs.ManagedBackupRecipient{}, err
	}
	return jobs.ManagedBackupRecipient{
		Public: recipient.Public, Fingerprint: recipient.Fingerprint, Generation: recipient.Generation, SiteRef: recipient.SiteRef, NodeRef: recipient.NodeRef,
		Statement: recipient.Statement, Challenge: recipient.Challenge,
	}, nil
}

func (adapter managedBackupDeliveryAdapter) SealReplacement(ctx context.Context, scope jobs.ManagedBackupReplacementScope, recipient jobs.ManagedBackupRecipient, operation string) ([]byte, error) {
	sealOperation := hostdelivery.OperationBackupTargetImport
	if operation == jobs.StackKitLifecycleBackupTargetRebind {
		sealOperation = hostdelivery.OperationBackupTargetRebind
	}
	return adapter.delivery.SealReplacement(ctx, managedstackkit.ReplacementScope{
		TenantID:                      scope.TenantID,
		StackID:                       scope.StackID,
		OwnerID:                       scope.OwnerID,
		ReplacementID:                 scope.ReplacementID,
		DestinationLeaseID:            scope.DestinationLeaseID,
		DestinationServerID:           scope.DestinationServerID,
		DestinationResourceGeneration: scope.DestinationResourceGeneration,
	}, managedstackkit.Recipient{
		Public: recipient.Public, Fingerprint: recipient.Fingerprint, Generation: recipient.Generation, SiteRef: recipient.SiteRef, NodeRef: recipient.NodeRef,
		Statement: recipient.Statement, Challenge: recipient.Challenge, PlacementRef: kombifywallet.PlacementRef(recipient.AgentID),
	}, sealOperation)
}

func (adapter managedBackupDeliveryAdapter) RecordReplacementNodeBinding(ctx context.Context, scope jobs.ManagedBackupReplacementScope, binding jobs.ManagedBackupNodeBinding) error {
	return adapter.delivery.RecordReplacementNodeBinding(ctx, managedstackkit.ReplacementScope{
		TenantID:                      scope.TenantID,
		StackID:                       scope.StackID,
		OwnerID:                       scope.OwnerID,
		ReplacementID:                 scope.ReplacementID,
		DestinationLeaseID:            scope.DestinationLeaseID,
		DestinationServerID:           scope.DestinationServerID,
		DestinationResourceGeneration: scope.DestinationResourceGeneration,
	}, backupstore.NodeBinding{BackupTargetRef: binding.BackupTargetRef, CustodyAttestationRef: binding.CustodyAttestationRef})
}

func (adapter managedBackupDeliveryAdapter) ReplacementNodeBinding(ctx context.Context, tenantID, stackID, replacementID string) (jobs.ManagedBackupNodeBinding, error) {
	binding, err := adapter.delivery.ReplacementNodeBinding(ctx, tenantID, stackID, replacementID)
	if err != nil {
		return jobs.ManagedBackupNodeBinding{}, err
	}
	return jobs.ManagedBackupNodeBinding{BackupTargetRef: binding.BackupTargetRef, CustodyAttestationRef: binding.CustodyAttestationRef}, nil
}

func (adapter managedBackupDeliveryAdapter) CommitReplacementCustody(ctx context.Context, tenantID, stackID, replacementID string) error {
	return adapter.delivery.CommitReplacementCustody(ctx, tenantID, stackID, replacementID)
}

func (adapter managedBackupDeliveryAdapter) DiscardReplacementCustody(ctx context.Context, tenantID, stackID, replacementID string) error {
	return adapter.delivery.DiscardReplacementCustody(ctx, tenantID, stackID, replacementID)
}
