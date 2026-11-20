package guardreach

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The homelab identity agent link (HOMELAB-IDENTITY-ACCESS-TARGET section 7,
// Gateway ADR 0042 "Agent node envelope contract"). The Gateway answers the
// Connect registration of a server with the node's agent envelope
// verification key (agent_envelope). Homelab checks it against the
// registration, keeps it on the server's reach row and delivers it, with the
// server, the Connect tenant, the node owner's subject and the D10-linked
// household members, to the node over the Guard (StackKit AGENT_LINK_INSTALL).
// The node pins it and verifies agent envelopes against it; the key is public
// and the references are opaque subjects, so nothing here is a credential.
const (
	agentEnvelopeAlg        = "EdDSA"
	agentEnvelopeTyp        = "kombify-agent-envelope+jwt"
	agentEnvelopeIss        = "kombify-gateway"
	agentLinkRequestSchema  = "stackkit.home-agent-link-request/v1"
	agentLinkMaxMembers     = 63
	agentLinkDeliverTimeout = 45 * time.Second
	// agentKeyRefresh is how often a registered server hands a fresh host
	// proof so the Gateway answers with its current key: a new Gateway master
	// key id reaches the node through the same path within a day.
	agentKeyRefresh = 24 * time.Hour
	// A new key or owner waits for the Owner's passkey step-up on the node:
	// dispatched again every agentLinkOwnerPoll while the node keeps the
	// request open (five minutes), raised again after agentLinkOwnerBackoff.
	agentLinkOwnerPoll    = 20 * time.Second
	agentLinkOwnerWindow  = 6 * time.Minute
	agentLinkOwnerBackoff = 6 * time.Hour

	ReasonAgentKeyInvalid         = "agent_envelope_key_invalid"
	ReasonAgentLinkMembers        = "agent_link_members_unavailable"
	ReasonAgentLinkDelivery       = "agent_link_delivery_failed"
	ReasonAgentLinkRemoveFailed   = "agent_link_remove_unreachable"
	ReasonAgentLinkOwnerPending   = "agent_link_owner_approval_pending"
	ReasonAgentLinkOwnerExpired   = "agent_link_owner_approval_expired"
	agentLinkDigestPrefix         = "sha256:"
	agentLinkUsernamePatternText  = `^[a-z0-9][a-z0-9._-]{0,63}$`
	agentEnvelopeKeyIDPatternText = `^[A-Za-z0-9._-]{1,48}\.([1-9][0-9]{0,17})$`
)

var (
	agentEnvelopeNodeID   = regexp.MustCompile(`^tsn_[0-9A-HJKMNP-TV-Z]{26}$`)
	agentEnvelopeKeyID    = regexp.MustCompile(agentEnvelopeKeyIDPatternText)
	agentLinkUsername     = regexp.MustCompile(agentLinkUsernamePatternText)
	errAgentKeyInvalid    = errors.New("guardreach: the Gateway agent envelope key does not match its registration")
	errAgentLinkUnwritten = errors.New("guardreach: the agent link state was not recorded")
	// ErrAgentLinkOwnerPending is the node's answer while a new key or owner
	// waits for the Owner's passkey step-up in the Hub.
	ErrAgentLinkOwnerPending = errors.New("guardreach: the agent link waits for the Owner's passkey approval on the node")
)

// AgentEnvelopeJWK is the public Ed25519 key of the key document.
type AgentEnvelopeJWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
}

// AgentEnvelopeKey is the Gateway's agent_envelope member, field for field.
type AgentEnvelopeKey struct {
	Alg               string           `json:"alg"`
	Typ               string           `json:"typ"`
	Iss               string           `json:"iss"`
	Aud               string           `json:"aud"`
	Kid               string           `json:"kid"`
	JWK               AgentEnvelopeJWK `json:"jwk"`
	JKT               string           `json:"jkt"`
	BindingGeneration int64            `json:"binding_generation"`
}

// Validate checks the key document against the registration it came with:
// the node id is the audience, the generation is the binding generation (and
// the kid suffix), and jkt is the RFC 7638 thumbprint of the Ed25519 key.
func (k AgentEnvelopeKey) Validate(nodeID string, generation int64) error {
	match := agentEnvelopeKeyID.FindStringSubmatch(k.Kid)
	raw, err := base64.RawURLEncoding.Strict().DecodeString(k.JWK.X)
	switch {
	case k.Alg != agentEnvelopeAlg, k.Typ != agentEnvelopeTyp, k.Iss != agentEnvelopeIss,
		!agentEnvelopeNodeID.MatchString(k.Aud), k.Aud != nodeID,
		k.BindingGeneration < 1, k.BindingGeneration != generation,
		match == nil, match[1] != strconv.FormatInt(k.BindingGeneration, 10),
		k.JWK.Kty != "OKP", k.JWK.Crv != "Ed25519", err != nil, len(raw) != 32,
		k.JKT != agentEnvelopeThumbprint(k.JWK.X):
		return errAgentKeyInvalid
	}
	return nil
}

func agentEnvelopeThumbprint(x string) string {
	digest := sha256.Sum256([]byte(`{"crv":"Ed25519","kty":"OKP","x":"` + x + `"}`))
	return base64.RawURLEncoding.EncodeToString(digest[:])
}

// AgentLinkMember is one household member whose D10 link is verified: the
// kombify (Auth0) subject and the Pocket ID username on the homelab.
type AgentLinkMember struct {
	CloudSubject string `json:"cloudSubject"`
	Username     string `json:"username"`
}

// AgentLinkDelivery reaches the node of one reach row over its Guard and reads
// the D10-linked household members of the row's stack. Install and Remove run
// the StackKit operations AGENT_LINK_INSTALL and AGENT_LINK_REMOVE on the
// row's own Guard worker and return only after the node's result verified.
type AgentLinkDelivery interface {
	LinkedMembers(ctx context.Context, row Row) ([]AgentLinkMember, error)
	Install(ctx context.Context, row Row, request []byte) error
	Remove(ctx context.Context, row Row) error
}

type agentLinkRequest struct {
	Schema        string            `json:"schema"`
	ServerID      string            `json:"serverId"`
	TenantID      string            `json:"tenantId"`
	AgentEnvelope json.RawMessage   `json:"agentEnvelope"`
	OwnerSubject  string            `json:"ownerSubject"`
	Members       []AgentLinkMember `json:"members"`
}

// AgentLinkRequest builds the exact request bytes for a row: the stored key,
// the server, the Connect tenant (the envelope's tid), the owner (the only
// person the Gateway issues envelopes for on this node) and the members in a
// stable order. A member without a valid username, the owner's own subject
// and duplicates are left out; the node maps each to its account.
func AgentLinkRequest(row Row, members []AgentLinkMember) ([]byte, error) {
	if row.AgentKey == "" || row.ServerID == "" || row.ConnectTenantID == "" || row.OwnerSubjectID == "" {
		return nil, errAgentKeyInvalid
	}
	seenSubject, seenUser := map[string]bool{row.OwnerSubjectID: true}, map[string]bool{}
	selected := make([]AgentLinkMember, 0, len(members))
	for _, member := range members {
		member.CloudSubject, member.Username = strings.TrimSpace(member.CloudSubject), strings.TrimSpace(member.Username)
		if member.CloudSubject == "" || !agentLinkUsername.MatchString(member.Username) || seenSubject[member.CloudSubject] || seenUser[member.Username] {
			continue
		}
		seenSubject[member.CloudSubject], seenUser[member.Username] = true, true
		selected = append(selected, member)
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Username < selected[j].Username })
	if len(selected) > agentLinkMaxMembers {
		selected = selected[:agentLinkMaxMembers]
	}
	return json.Marshal(agentLinkRequest{
		Schema: agentLinkRequestSchema, ServerID: row.ServerID, TenantID: row.ConnectTenantID,
		AgentEnvelope: json.RawMessage(row.AgentKey), OwnerSubject: row.OwnerSubjectID, Members: selected,
	})
}

func agentLinkDigest(request []byte) string {
	digest := sha256.Sum256(request)
	return agentLinkDigestPrefix + hex.EncodeToString(digest[:])
}

// agentKeyRefreshWanted reports a registered server whose key was last read
// more than agentKeyRefresh ago: the Guard is asked for one more Connect host
// proof and the registration is repeated to read the Gateway's current key.
func (r Row) agentKeyRefreshWanted(now time.Time) bool {
	if r.Desired != DesiredActive || r.Generation < 1 || r.ConnectTenantID == "" || !r.Connect.registeredAt(r.Generation) || r.Connect.UpdatedAt == nil {
		return false
	}
	if r.Connect.RetryAt != nil && now.Before(*r.Connect.RetryAt) {
		return false
	}
	return !now.Before(r.Connect.UpdatedAt.Add(agentKeyRefresh))
}

// storeAgentKey records the key document of a successful registration on the
// row. It returns true when the stored key changed, so the link is delivered
// again.
func storeAgentKey(r *Row, node *Node, now time.Time) bool {
	key := ""
	if node != nil && node.AgentEnvelope != nil {
		if raw, err := json.Marshal(node.AgentEnvelope); err == nil {
			key = string(raw)
		}
	}
	if node != nil && node.AgentEnvelopeInvalid {
		r.AgentLink = Target{State: TargetFailed, Generation: r.Generation, ReasonCode: ReasonAgentKeyInvalid, UpdatedAt: &now}
	}
	if key == r.AgentKey {
		return false
	}
	r.AgentKey = key
	if key != "" {
		r.AgentLink = Target{State: TargetPending, Generation: r.Generation, Ref: r.AgentLink.Ref, UpdatedAt: &now}
	}
	return true
}

// deliverAgentLink brings the node's pin to the row's current key and linked
// members. A request the node already pinned for this generation is not sent
// again; a failed delivery of the same request waits for its retry time. A new
// key or owner needs the Owner's passkey step-up on the node: while it is open
// the request is dispatched again every agentLinkOwnerPoll so the node picks
// up the approval within its 60 s window; a step-up the Owner left unanswered
// for agentLinkOwnerWindow is raised again only after agentLinkOwnerBackoff.
func (s *Service) deliverAgentLink(ctx context.Context, tenantID, workerID string) error {
	if s.cfg.AgentLinks == nil {
		return nil
	}
	row, err := s.cfg.Store.Get(ctx, tenantID, workerID)
	if err != nil {
		return err
	}
	if row.Desired != DesiredActive || row.AgentKey == "" || !row.Connect.registeredAt(row.Generation) {
		return nil
	}
	now := s.now()
	record := func(target Target, pinned string, due *time.Time) error {
		_, err := s.cfg.Store.Update(ctx, tenantID, workerID, func(r *Row) bool {
			// What the node pinned is recorded whatever happened meanwhile,
			// so a removal always knows there is a pin to remove.
			changed := false
			if pinned != "" && r.AgentLinkPinned != pinned {
				r.AgentLinkPinned, changed = pinned, true
			}
			// A new binding or key while the delivery ran supersedes the rest.
			if r.Generation != row.Generation || r.AgentKey != row.AgentKey || r.Desired != DesiredActive {
				return changed
			}
			r.AgentLink = target
			if due != nil {
				r.NextAttemptAt = *due
			}
			return true
		})
		return err
	}
	members, err := s.cfg.AgentLinks.LinkedMembers(ctx, *row)
	if err != nil {
		retry := now.Add(connectRetryable)
		return record(Target{State: TargetFailed, Generation: row.Generation, Ref: row.AgentLink.Ref, ReasonCode: ReasonAgentLinkMembers, UpdatedAt: &now, RetryAt: &retry}, "", nil)
	}
	request, err := AgentLinkRequest(*row, members)
	if err != nil {
		return record(Target{State: TargetFailed, Generation: row.Generation, ReasonCode: ReasonAgentKeyInvalid, UpdatedAt: &now}, "", nil)
	}
	digest := agentLinkDigest(request)
	current := row.AgentLink
	same := current.Generation == row.Generation && current.Ref == digest
	waiting := current.RetryAt != nil && now.Before(*current.RetryAt)
	if same && (current.State == TargetRegistered || ((current.State == TargetFailed || current.State == TargetPending) && waiting)) {
		return nil
	}
	callCtx, cancel := context.WithTimeout(ctx, agentLinkDeliverTimeout)
	installErr := s.cfg.AgentLinks.Install(callCtx, *row, request)
	cancel()
	switch {
	case errors.Is(installErr, ErrAgentLinkOwnerPending):
		// The window starts with the first pending answer for this request.
		started := now
		if same && current.State == TargetPending && current.UpdatedAt != nil {
			started = *current.UpdatedAt
		}
		if now.Sub(started) >= agentLinkOwnerWindow {
			retry := now.Add(agentLinkOwnerBackoff)
			return record(Target{State: TargetPending, Generation: row.Generation, Ref: digest, ReasonCode: ReasonAgentLinkOwnerExpired, UpdatedAt: &now, RetryAt: &retry}, "", nil)
		}
		retry := now.Add(agentLinkOwnerPoll)
		return record(Target{State: TargetPending, Generation: row.Generation, Ref: digest, ReasonCode: ReasonAgentLinkOwnerPending, UpdatedAt: &started, RetryAt: &retry}, "", &retry)
	case installErr != nil:
		s.log.Warn("guard_reach_agent_link_failed", "worker_id", workerID, "error", installErr.Error())
		retry := now.Add(connectRetryable)
		if current.State == TargetFailed && current.Ref == digest {
			retry = now.Add(connectPermanent)
		}
		return record(Target{State: TargetFailed, Generation: row.Generation, Ref: digest, ReasonCode: ReasonAgentLinkDelivery, UpdatedAt: &now, RetryAt: &retry}, "", nil)
	}
	return record(Target{State: TargetRegistered, Generation: row.Generation, Ref: digest, UpdatedAt: &now}, digest, nil)
}

// removeAgentLink deletes the node's pin while the Guard still answers: on a
// detach before the Connect revocation, and on a Gateway revocation event
// before the Guard's access ends. It runs whenever the node pinned a link
// (whatever the delivery state says) and is tried once per pinned request; a
// Guard that no longer answers leaves the pin, and the Gateway issues no
// envelope for a revoked node anyway. It returns the target to record.
func (s *Service) removeAgentLink(ctx context.Context, row Row) (Target, bool) {
	if s.cfg.AgentLinks == nil || row.AgentLinkPinned == "" ||
		(row.AgentLink.State == TargetFailed && row.AgentLink.ReasonCode == ReasonAgentLinkRemoveFailed && row.AgentLink.Ref == row.AgentLinkPinned) {
		return row.AgentLink, false
	}
	now := s.now()
	callCtx, cancel := context.WithTimeout(ctx, agentLinkDeliverTimeout)
	err := s.cfg.AgentLinks.Remove(callCtx, row)
	cancel()
	if err != nil {
		s.log.Warn("guard_reach_agent_link_remove_failed", "worker_id", row.WorkerID, "error", err.Error())
		return Target{State: TargetFailed, Generation: row.AgentLink.Generation, Ref: row.AgentLinkPinned, ReasonCode: ReasonAgentLinkRemoveFailed, UpdatedAt: &now}, true
	}
	return Target{State: TargetRevoked, Generation: row.AgentLink.Generation, Ref: row.AgentLinkPinned, UpdatedAt: &now}, true
}

// recordAgentLinkRemoval stores the outcome of removeAgentLink; a removed
// link clears the pin record.
func (s *Service) recordAgentLinkRemoval(ctx context.Context, row Row, target Target) error {
	_, err := s.cfg.Store.Update(ctx, row.TenantID, row.WorkerID, func(r *Row) bool {
		if r.AgentLinkPinned != row.AgentLinkPinned {
			return false
		}
		r.AgentLink = target
		if target.State == TargetRevoked {
			r.AgentLinkPinned = ""
		}
		return true
	})
	if err != nil {
		return fmt.Errorf("%w: %w", errAgentLinkUnwritten, err)
	}
	return nil
}
