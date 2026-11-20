package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Identity origin sources of StackKits' #IdentityOriginV1 (homelab identity
// target §5, §14.2). The closed vocabulary is enforced here because migration
// 185 is additive and adds no constraint to homelabs.
const (
	HomelabIdentityOriginKombifyMe    = "kombify-me"
	HomelabIdentityOriginOwnDomain    = "own-domain"
	HomelabIdentityOriginLocalOnly    = "local-only"
	HomelabIdentityOriginReservedName = "reserved-name"
)

// HomelabIdentityOrigin is the homelab's identity origin: the Pocket ID host
// id.<Name>, which is the passkey relying party, and how StackKits classifies
// that name.
type HomelabIdentityOrigin struct {
	Name   string
	Source string
}

// Host is the Pocket ID host of the origin (#IdentityOriginV1 host).
func (o HomelabIdentityOrigin) Host() string { return "id." + o.Name }

// Certificates is how the origin's certificates are trusted
// (#IdentityOriginV1 certificates): a kombify.me name or an own domain carries
// publicly trusted certificates, lab.home and a reserved name only the
// homelab root.
func (o HomelabIdentityOrigin) Certificates() string {
	if o.Source == HomelabIdentityOriginLocalOnly || o.Source == HomelabIdentityOriginReservedName {
		return "homelab-root"
	}
	return "publicly-trusted"
}

// HomelabIdentityAuthority is the authority pointer of ADR-036: the server
// that runs the homelab's one Pocket ID, the StackKit deployment that runs it
// there, and the identity origin it serves. The origin is what the
// Authority's Guard reported; nothing has verified it against the kombify.me
// registry or DNS yet.
type HomelabIdentityAuthority struct {
	ServerID   string
	StackID    string
	Origin     HomelabIdentityOrigin
	ObservedAt time.Time
	// Departed reports, at read time, that the Authority server left the
	// homelab (decommissioned, its deployment deleted or moved). The pointer
	// stays until a successor with the same origin, a restore, takes over.
	Departed bool
}

// HomelabIdentityObservation is what one Guard inventory says about the
// identity of its own server. RunsPocketID with Origin: the applied StackKit
// runs the managed Pocket ID for this origin. Without RunsPocketID: the
// StackKit declares services and Pocket ID is none of them. It names no
// server, stack or homelab; the projection takes those from Homelab's own
// records of the accepted Guard.
type HomelabIdentityObservation struct {
	RunsPocketID bool
	Origin       HomelabIdentityOrigin
}

// HomelabIdentityOriginOf derives the identity origin from the homelab name
// the StackKit plan carries (network domain base), exactly as StackKits'
// compiler derives #IdentityOriginV1: lab.home is the local-only origin, a
// special-use or reserved name is reserved-name, a kombify.me name is
// kombify-me and every other name is the owner's own domain. A name that is
// not a DNS name yields false.
func HomelabIdentityOriginOf(name string) (HomelabIdentityOrigin, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !homelabIdentityDNSName(name) {
		return HomelabIdentityOrigin{}, false
	}
	origin := HomelabIdentityOrigin{Name: name}
	switch {
	case name == "lab.home":
		origin.Source = HomelabIdentityOriginLocalOnly
	case homelabIdentityReservedName(name):
		origin.Source = HomelabIdentityOriginReservedName
	case strings.HasSuffix(name, ".kombify.me"):
		origin.Source = HomelabIdentityOriginKombifyMe
	default:
		origin.Source = HomelabIdentityOriginOwnDomain
	}
	return origin, true
}

// homelabIdentityReservedName mirrors #IdentityOriginReservedNamePattern.
func homelabIdentityReservedName(name string) bool {
	for _, suffix := range []string{"localhost", "local", "internal", "arpa", "test", "invalid", "example", "home", "lan"} {
		if name == suffix || strings.HasSuffix(name, "."+suffix) {
			return true
		}
	}
	return false
}

// homelabIdentityDNSName admits a lower-case DNS name whose Pocket ID host
// id.<name> still fits 253 octets: labels of 1 to 63 letters, digits and
// inner hyphens, no empty label and no trailing dot.
func homelabIdentityDNSName(name string) bool {
	if name == "" || len(name) > 250 {
		return false
	}
	for _, label := range strings.Split(name, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
	}
	return true
}

// IsPocketIDServiceName reports whether a service key or name names the
// StackKits-managed Pocket ID, by the rule the household worker's
// RunsPocketID applies to the service inventory (platform-n3dcz): lower case,
// spaces and hyphens as underscores, pocket_id or pocketid.
func IsPocketIDServiceName(value string) bool {
	normalized := strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(value)))
	return normalized == "pocket_id" || normalized == "pocketid"
}

// homelabIdentityAuthorityPresentSQL is true while the pointed-to Authority
// server is still part of its homelab: not decommissioning or decommissioned,
// on a live deployment of that homelab. The homelab reads select it as the
// last homelab column.
const homelabIdentityAuthorityPresentSQL = `EXISTS (SELECT 1 FROM servers a
			JOIN stacks ast ON ast.tenant_id = a.tenant_id AND ast.id = a.stack_id
			WHERE a.tenant_id = homelabs.tenant_id AND a.id = homelabs.identity_authority_server_id
				AND a.lifecycle_state NOT IN ('decommissioning', 'decommissioned')
				AND ast.deleted_at IS NULL AND ast.homelab_id = homelabs.id)`

// homelabIdentityAuthorityFromColumns rebuilds the pointer from its columns.
// A partial or out-of-vocabulary row reads as no Authority, never as a
// guessed one.
func homelabIdentityAuthorityFromColumns(serverID, stackID, originName, originSource sql.NullString, observedAt sql.NullTime, present bool) *HomelabIdentityAuthority {
	if !serverID.Valid || !stackID.Valid || !originName.Valid || !originSource.Valid || !observedAt.Valid {
		return nil
	}
	origin, ok := HomelabIdentityOriginOf(originName.String)
	if !ok || origin.Name != originName.String || origin.Source != originSource.String ||
		strings.TrimSpace(serverID.String) == "" || strings.TrimSpace(stackID.String) == "" {
		return nil
	}
	return &HomelabIdentityAuthority{
		ServerID: serverID.String, StackID: stackID.String, Origin: origin,
		ObservedAt: observedAt.Time.UTC(), Departed: !present,
	}
}

func cloneHomelabIdentityAuthority(authority *HomelabIdentityAuthority) *HomelabIdentityAuthority {
	if authority == nil {
		return nil
	}
	clone := *authority
	return &clone
}

// homelabIdentityCandidate is one server's standing for the Authority, all
// from Homelab's records: the homelab its live deployment belongs to and
// that deployment's and server's age.
type homelabIdentityCandidate struct {
	ServerID, StackID, HomelabID string
	StackCreated, ServerCreated  time.Time
}

// precedes orders candidates by Homelab's records, never by report order:
// the homelab's earliest StackKit deployment, then its earliest server, then
// the server id. The first StackKit deployment of a homelab founds it
// (ADR-036), so its Pocket ID is the Authority.
func (c homelabIdentityCandidate) precedes(other homelabIdentityCandidate) bool {
	if !c.StackCreated.Equal(other.StackCreated) {
		return c.StackCreated.Before(other.StackCreated)
	}
	if !c.ServerCreated.Equal(other.ServerCreated) {
		return c.ServerCreated.Before(other.ServerCreated)
	}
	return c.ServerID < other.ServerID
}

// homelabIdentityTakesOver decides whether a server that reports the managed
// Pocket ID becomes, or stays, its homelab's Authority:
//   - no Authority yet, or the Authority itself reporting: yes;
//   - the Authority is still in the homelab: only a candidate that precedes it
//     by Homelab's records, so the outcome does not depend on which Guard
//     reported first;
//   - the Authority left the homelab: only a successor that serves the same
//     identity origin, a restore of the Authority (passkeys bind to the
//     origin, target §9). Any other successor waits for the governed
//     operation that moves the Authority (P2); the pointer then reads as
//     departed.
func homelabIdentityTakesOver(current *HomelabIdentityAuthority, currentRank *homelabIdentityCandidate, candidate homelabIdentityCandidate, origin HomelabIdentityOrigin) bool {
	switch {
	case current == nil, current.ServerID == candidate.ServerID:
		return true
	case currentRank != nil:
		return candidate.precedes(*currentRank)
	default:
		return current.Origin == origin
	}
}

// projectHomelabIdentityAuthority records the reporting server's identity
// facts on its homelab. It runs inside the fenced Guard inventory satellite
// transaction, after the server row of exactly this accepted Guard
// observation was locked, so a stale or replayed report never reaches it;
// the homelab row is locked before the decision so concurrent claims
// serialize.
//
// Every binding comes from Homelab's records, never from the report: the
// server's own live deployment, that deployment's homelab, the same tenant
// and owner. The Authority's own reports keep its deployment and origin
// current (stackkit migrate-domain), and an Authority whose StackKit declares
// services without Pocket ID releases the pointer. Nothing is written while
// nothing changes.
func projectHomelabIdentityAuthority(ctx context.Context, tx *sql.Tx, tenantID, serverID string, observation HomelabIdentityObservation, observedAt time.Time) error {
	if !observation.RunsPocketID {
		_, err := tx.ExecContext(ctx, `
			UPDATE homelabs
			SET identity_authority_server_id = NULL, identity_authority_stack_id = NULL,
				identity_origin_name = NULL, identity_origin_source = NULL,
				identity_authority_observed_at = NULL
			WHERE tenant_id = $1 AND identity_authority_server_id = $2 AND deleted_at IS NULL
		`, tenantID, serverID)
		return err
	}
	origin, ok := HomelabIdentityOriginOf(observation.Origin.Name)
	if !ok || origin != observation.Origin {
		return nil
	}
	var (
		candidate                                               homelabIdentityCandidate
		currentServer, currentStack, currentName, currentSource sql.NullString
		currentObserved                                         sql.NullTime
	)
	err := tx.QueryRowContext(ctx, `
		SELECT sv.id, st.id, h.id, st.created_at, sv.created_at,
			h.identity_authority_server_id, h.identity_authority_stack_id,
			h.identity_origin_name, h.identity_origin_source, h.identity_authority_observed_at
		FROM servers sv
		JOIN stacks st ON st.tenant_id = sv.tenant_id AND st.id = sv.stack_id
		JOIN homelabs h ON h.tenant_id = st.tenant_id AND h.id = st.homelab_id
		WHERE sv.tenant_id = $1 AND sv.id = $2
			AND sv.lifecycle_state NOT IN ('decommissioning', 'decommissioned')
			AND st.deleted_at IS NULL AND h.deleted_at IS NULL
			AND h.owner_subject_id = sv.owner_subject_id AND h.owner_subject_id = st.owner_subject_id
		FOR UPDATE OF h
	`, tenantID, serverID).Scan(&candidate.ServerID, &candidate.StackID, &candidate.HomelabID,
		&candidate.StackCreated, &candidate.ServerCreated,
		&currentServer, &currentStack, &currentName, &currentSource, &currentObserved)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	current := homelabIdentityAuthorityFromColumns(currentServer, currentStack, currentName, currentSource, currentObserved, true)
	if current != nil && current.ServerID == candidate.ServerID && current.StackID == candidate.StackID && current.Origin == origin {
		return nil
	}
	var currentRank *homelabIdentityCandidate
	if current != nil && current.ServerID != candidate.ServerID {
		rank := homelabIdentityCandidate{ServerID: current.ServerID}
		err := tx.QueryRowContext(ctx, `
			SELECT a.created_at, ast.created_at
			FROM servers a
			JOIN stacks ast ON ast.tenant_id = a.tenant_id AND ast.id = a.stack_id
			WHERE a.tenant_id = $1 AND a.id = $2
				AND a.lifecycle_state NOT IN ('decommissioning', 'decommissioned')
				AND ast.deleted_at IS NULL AND ast.homelab_id = $3
		`, tenantID, current.ServerID, candidate.HomelabID).Scan(&rank.ServerCreated, &rank.StackCreated)
		switch {
		case err == nil:
			currentRank = &rank
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
	}
	if !homelabIdentityTakesOver(current, currentRank, candidate, origin) {
		return nil
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE homelabs
		SET identity_authority_server_id = $3, identity_authority_stack_id = $4,
			identity_origin_name = $5, identity_origin_source = $6,
			identity_authority_observed_at = $7
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
	`, tenantID, candidate.HomelabID, candidate.ServerID, candidate.StackID, origin.Name, origin.Source, observedAt.UTC())
	return err
}

// projectHomelabIdentityAuthorityLocked is the MemoryStore form of
// projectHomelabIdentityAuthority. The caller holds s.mu for writing and has
// matched the server head to the accepted Guard observation.
func (s *MemoryStore) projectHomelabIdentityAuthorityLocked(tenantID, serverID string, observation HomelabIdentityObservation, observedAt time.Time) {
	if !observation.RunsPocketID {
		for id, homelab := range s.homelabs {
			if homelab.TenantID == tenantID && homelab.DeletedAt == nil &&
				homelab.IdentityAuthority != nil && homelab.IdentityAuthority.ServerID == serverID {
				homelab.IdentityAuthority = nil
				homelab.UpdatedAt = s.now()
				s.homelabs[id] = homelab
			}
		}
		return
	}
	origin, ok := HomelabIdentityOriginOf(observation.Origin.Name)
	if !ok || origin != observation.Origin {
		return
	}
	candidate, homelab, ok := s.memoryIdentityCandidateLocked(tenantID, serverID, "")
	if !ok || homelab.OwnerSubjectID != s.servers[serverID].OwnerSubjectID {
		return
	}
	current := homelab.IdentityAuthority
	if current != nil && current.ServerID == candidate.ServerID && current.StackID == candidate.StackID && current.Origin == origin {
		return
	}
	var currentRank *homelabIdentityCandidate
	if current != nil && current.ServerID != candidate.ServerID {
		if rank, _, inHomelab := s.memoryIdentityCandidateLocked(tenantID, current.ServerID, homelab.ID); inHomelab {
			currentRank = &rank
		}
	}
	if !homelabIdentityTakesOver(current, currentRank, candidate, origin) {
		return
	}
	homelab.IdentityAuthority = &HomelabIdentityAuthority{
		ServerID: candidate.ServerID, StackID: candidate.StackID, Origin: origin, ObservedAt: observedAt.UTC(),
	}
	homelab.UpdatedAt = s.now()
	s.homelabs[homelab.ID] = homelab
}

// memoryIdentityCandidateLocked resolves a live server's standing from the
// store's records. With homelabID set the server must belong to that homelab.
func (s *MemoryStore) memoryIdentityCandidateLocked(tenantID, serverID, homelabID string) (homelabIdentityCandidate, Homelab, bool) {
	server, ok := s.servers[serverID]
	if !ok || server.TenantID != tenantID || server.LifecycleState == "decommissioning" || server.LifecycleState == "decommissioned" {
		return homelabIdentityCandidate{}, Homelab{}, false
	}
	stack, ok := s.stacks[server.StackID]
	if !ok || stack.TenantID != tenantID || stack.DeletedAt != nil || stack.HomelabID == "" ||
		(homelabID != "" && stack.HomelabID != homelabID) {
		return homelabIdentityCandidate{}, Homelab{}, false
	}
	homelab, ok := s.homelabs[stack.HomelabID]
	if !ok || homelab.TenantID != tenantID || homelab.DeletedAt != nil || homelab.OwnerSubjectID != stack.OwnerSubjectID {
		return homelabIdentityCandidate{}, Homelab{}, false
	}
	return homelabIdentityCandidate{
		ServerID: server.ID, StackID: stack.ID, HomelabID: homelab.ID,
		StackCreated: stack.CreatedAt, ServerCreated: server.CreatedAt,
	}, homelab, true
}

// memoryHomelabViewLocked marks a departed Authority on a homelab read, as
// the Postgres reads do in SQL.
func (s *MemoryStore) memoryHomelabViewLocked(homelab Homelab) *Homelab {
	view := cloneHomelab(homelab)
	if view.IdentityAuthority != nil {
		_, _, present := s.memoryIdentityCandidateLocked(homelab.TenantID, view.IdentityAuthority.ServerID, homelab.ID)
		view.IdentityAuthority.Departed = !present
	}
	return view
}
