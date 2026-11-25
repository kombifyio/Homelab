package reachagent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/kombifyio/homelab/pkg/guardidentity"
	"github.com/kombifyio/homelab/pkg/guardreach"
)

type PaperworkNodeCustody interface {
	PaperworkRecipient(context.Context, string) (json.RawMessage, error)
	ApplyPaperworkSignIn(context.Context, guardreach.PaperworkSignInTask) error
}

func (a *Agent) answerPaperworkSignIn(ctx context.Context, document *guardreach.Document) *guardreach.PaperworkSignInReport {
	task, relay := document.PaperworkSignIn, document.KombifyMe
	if task == nil || a.cfg.PaperworkSignIn == nil || task.ServerID != a.cfg.ServerID || task.TenantID != a.cfg.TenantID ||
		task.Generation != document.Generation || task.Generation < 1 {
		return nil
	}
	if task.Kind != "revoke" && (relay == nil || !a.acceptTarget(relay) || task.Generation != relay.Generation ||
		relay.TargetPort != 8080 || task.Origin != "https://"+relay.Address) {
		return nil
	}
	reservation, err := uuid.Parse(task.Reservation)
	if err != nil || reservation.Version() != 4 || reservation.String() != task.Reservation {
		return nil
	}
	command := guardreach.PaperworkSignInCommand{Kind: task.Kind, Reservation: task.Reservation, Origin: task.Origin, ServerID: task.ServerID}
	switch task.Kind {
	case "enable":
		recipient, err := a.cfg.PaperworkSignIn.PaperworkRecipient(ctx, task.Reservation)
		if err != nil {
			return nil
		}
		command.OwnerApproved, command.Recipient = true, recipient
	case "apply":
		if len(task.Envelope) == 0 || len(task.Envelope) > 32<<10 || a.cfg.PaperworkSignIn.ApplyPaperworkSignIn(ctx, *task) != nil {
			return nil
		}
		digest := sha256.Sum256([]byte(task.Envelope))
		command.Kind, command.EnvelopeSHA256 = "ack", hex.EncodeToString(digest[:])
	case "read", "revoke":
	default:
		return nil
	}
	body, err := json.Marshal(command)
	if err != nil {
		return nil
	}
	proof, err := a.cfg.Manager.MintPaperworkSignInProof(guardidentity.HostProofInput{
		Issuer: a.cfg.WorkerID, Subject: a.cfg.ServerID, TenantID: task.TenantID, Generation: task.Generation, Now: a.cfg.Now(),
	}, body)
	if err != nil {
		return nil
	}
	return &guardreach.PaperworkSignInReport{Body: string(body), Proof: proof}
}
