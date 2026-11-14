package jobs

import "strings"

// IsStackKitBackupRead reports the one backup operation that only reads: the
// status of the node's local repository and its authenticated receipts. It
// carries no cost, changes nothing and opens no credential the node does not
// already hold, so it is not part
// of the managed backup custody that keeps run, configure and restore with the
// kit's own schedule and the owner's entitlement.
func IsStackKitBackupRead(operation string) bool {
	return strings.ToLower(strings.TrimSpace(operation)) == StackKitLifecycleBackupStatus
}

// BackupNotReadableError refuses a backup read for a server that has no backup
// the read could answer, before any Decision, lease or claim exists. The
// refusal is a fact of the kit's layout, never a guess about configuration;
// whether a repository is configured on a node that could hold one is answered
// by the node itself (reason codes backup_not_configured and
// backup_source_unavailable in the card's evidence).
type BackupNotReadableError struct {
	StackKit string
	NodeRef  string
	// HoldingNodeRef is the node of the kit that keeps the backup.
	HoldingNodeRef string
}

func (e *BackupNotReadableError) Error() string {
	return "the backup of " + e.StackKit + " is made by node " + e.HoldingNodeRef + ", not by node " + e.NodeRef
}

// ReasonCode is the stable machine code of the refusal.
func (e *BackupNotReadableError) ReasonCode() string { return "backup_not_readable" }

// Guidance is the person-facing explanation and next steps.
func (e *BackupNotReadableError) Guidance() (title, body string, nextSteps []string) {
	return "This server keeps no backup to check",
		"The backup of this StackKit is made by the backup runtime of its home server (node " + e.HoldingNodeRef + "), not by this server (node " + e.NodeRef + ").",
		[]string{
			"Run the backup check on the home server of this stack (node " + e.HoldingNodeRef + ").",
			"To see what is backed up from every server of the stack, read the stack's backup status (get_backup_status).",
		}
}

// RequireBackupReadable refuses a backup read addressed to a node of a
// multi-node kit other than the one whose backup runtime makes the kit's
// backup. Today that is the Cloud edge of Modern Homelab: the backup runtime
// and its repository (local-backup-runtime, the required basement-core) belong
// to the home server, the kit's authority node.
func RequireBackupReadable(stackKit, nodeRef string) error {
	if IsStackKitAuthorityNode(stackKit, nodeRef) {
		return nil
	}
	return &BackupNotReadableError{StackKit: stackKit, NodeRef: strings.TrimSpace(nodeRef), HoldingNodeRef: PrimaryStackKitNodeRef(stackKit)}
}
