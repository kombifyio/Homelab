package routes

import (
	"net/http"

	"github.com/kombifyio/homelab/internal/gocommon/identity"
	"github.com/kombifyio/homelab/internal/routes/tenantguard"
	"github.com/kombifyio/homelab/pkg/api"
	"github.com/kombifyio/homelab/pkg/httpx"
)

func authenticatedUserID(e *httpx.Event) (string, bool) {
	userID, _, ok := authenticatedUser(e)
	return userID, ok
}

// authenticatedUser returns the signed-in subject, whether it is the instance
// owner, and whether a subject exists at all.
func authenticatedUser(e *httpx.Event) (string, bool, bool) {
	if e == nil {
		return "", false, false
	}
	if e.Auth != nil && e.Auth.Id != "" {
		return e.Auth.Id, identityIsInstanceOwner(e.Auth.Identity), true
	}
	if e.Request != nil {
		if id := identity.FromContext(e.Request.Context()); id != nil && id.IsAuthenticated() {
			return id.UserID, identityIsInstanceOwner(id), true
		}
	}
	return "", false, false
}

// identityIsInstanceOwner reports owner rights over one self-hosted
// installation: the local owner holds the "admin" membership of their own
// installation. The hosted product has no instance owner. A kombify platform
// role (admin, super_admin, global_admin, developer, manager) is not owner
// authority there: a signed-in person of the hosted product sees and does what
// their own subscription and their own data allow, whatever role the platform
// holds for them.
func identityIsInstanceOwner(id *identity.Identity) bool {
	if id == nil || tenantguard.Active() {
		return false
	}
	return id.HasRole("admin")
}

func requireInstanceOwner(e *httpx.Event) error {
	ok, err := requireInstanceOwnerAccess(e)
	if err != nil {
		return err
	}
	if !ok {
		return httpx.ErrResponseWritten
	}
	return nil
}

func requireInstanceOwnerAccess(e *httpx.Event) (bool, error) {
	if e != nil && e.Request != nil && identityIsInstanceOwner(identity.FromContext(e.Request.Context())) {
		return true, nil
	}
	return false, httpx.Reject(e, http.StatusForbidden, api.ErrCodeForbidden, "Instance owner access required", nil)
}
