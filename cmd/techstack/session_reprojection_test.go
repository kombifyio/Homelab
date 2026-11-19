package main

// Classification-matrix tests for the silent session-recovery seam
// (kombify-Techstack-nzy1.14): a signature-valid v2 session whose
// identity/tenant projection cannot resolve must first be re-projected
// server-side and only fail with the retryable 401
// session renewal signal (reason session_reprojection, plus a cleared session cookie) when
// unrecoverable. Genuine resource-level denials keep their 403 semantics
// (covered in internal/routes inventory tests).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kombifyio/techstack/internal/gocommon/authsession"
	"github.com/kombifyio/techstack/internal/routes/sessionreauth"
	"github.com/kombifyio/techstack/pkg/config"
	"github.com/kombifyio/techstack/pkg/controlplane"
	"github.com/kombifyio/techstack/pkg/httpx"
	"github.com/kombifyio/techstack/pkg/identity"
)

func sessionReprojectionTestEvent(t *testing.T, mgr *authsession.Manager, claims authsession.Claims) (*httpx.Event, *httptest.ResponseRecorder) {
	t.Helper()
	token, err := mgr.Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/stacks", nil)
	req.AddCookie(&http.Cookie{Name: "techstack_session", Value: token})
	recorder := httptest.NewRecorder()
	return &httpx.Event{Request: req, Response: recorder}, recorder
}

func clearedSessionCookie(t *testing.T, recorder *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	for _, cookie := range recorder.Result().Cookies() {
		if cookie.Name == "techstack_session" {
			return cookie
		}
	}
	return nil
}

// Matrix row 1: a valid projection passes through untouched - no error, no
// cookie mutation, identity keeps the session tenant.
func TestSessionReprojectionMatrixValidProjectionPassesThrough(t *testing.T) {
	sessionreauth.Configure("techstack_session", false)
	mgr := mustSessionManager(t)
	store := &stubAuthStore{membership: &controlplane.Membership{
		TenantID: "org_alive", UserID: "auth0|user-1", RoleKey: "member", Status: "active",
	}}
	event, recorder := sessionReprojectionTestEvent(t, mgr, authsession.Claims{
		Subject: "auth0|user-1", TenantID: "org_alive",
	})

	if err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: true})(event); err != nil {
		t.Fatalf("middleware error = %v, want pass-through", err)
	}
	id := identity.FromContext(event.Request.Context())
	if id == nil || id.OrgID != "org_alive" {
		t.Fatalf("identity = %+v, want session tenant org_alive", id)
	}
	if cookie := clearedSessionCookie(t, recorder); cookie != nil {
		t.Fatalf("session cookie mutated on valid projection: %+v", cookie)
	}
}

// Matrix row 2a: recoverable via membership re-derivation - the session is
// bound to a since-removed tenant, but the user still holds an active org
// membership; the request recovers in place without any client involvement.
func TestSessionReprojectionMatrixRecoversViaMembershipRederivation(t *testing.T) {
	sessionreauth.Configure("techstack_session", false)
	mgr := mustSessionManager(t)
	store := &stubAuthStore{memberships: []controlplane.Membership{{
		TenantID: "org_alive", UserID: "auth0|user-1", RoleKey: "member", Status: "active",
	}}}
	event, recorder := sessionReprojectionTestEvent(t, mgr, authsession.Claims{
		Subject: "auth0|user-1", TenantID: "org_gone",
	})

	if err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: true})(event); err != nil {
		t.Fatalf("middleware error = %v, want in-place recovery", err)
	}
	id := identity.FromContext(event.Request.Context())
	if id == nil || id.OrgID != "org_alive" {
		t.Fatalf("identity = %+v, want recovered tenant org_alive", id)
	}
	if cookie := clearedSessionCookie(t, recorder); cookie != nil {
		t.Fatalf("session cookie mutated on recovered projection: %+v", cookie)
	}
}

// Matrix row 2b: recoverable via owner-tenant bootstrap - the user has no
// remaining memberships at all (the runtime-db incident shape); SaaS rebinds
// the session to the canonical personal owner tenant and materializes it.
func TestSessionReprojectionMatrixRecoversViaOwnerTenantBootstrap(t *testing.T) {
	sessionreauth.Configure("techstack_session", false)
	mgr := mustSessionManager(t)
	store := &stubAuthStore{}
	event, recorder := sessionReprojectionTestEvent(t, mgr, authsession.Claims{
		Subject: "auth0|user-1", TenantID: "org_gone",
	})

	if err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: true})(event); err != nil {
		t.Fatalf("middleware error = %v, want owner-tenant bootstrap recovery", err)
	}
	id := identity.FromContext(event.Request.Context())
	if id == nil || id.OrgID != "usr:auth0|user-1" {
		t.Fatalf("identity = %+v, want owner tenant usr:auth0|user-1", id)
	}
	if len(store.tenants) == 0 || store.tenants[0].ID != "usr:auth0|user-1" {
		t.Fatalf("bootstrapped tenants = %#v, want owner tenant materialized", store.tenants)
	}
	if len(store.memberships) == 0 || store.memberships[0].TenantID != "usr:auth0|user-1" {
		t.Fatalf("bootstrapped memberships = %#v, want owner membership", store.memberships)
	}
	if cookie := clearedSessionCookie(t, recorder); cookie != nil {
		t.Fatalf("session cookie mutated on recovered projection: %+v", cookie)
	}
}

// Matrix row 3: unrecoverable - no memberships and no SaaS owner fallback.
// The middleware answers 401 + error_code=session_renewal_required (reason session_reprojection) +
// retryable=true and clears the v2 session cookie so the client's central
// interceptor can silently re-enter SSO.
func TestSessionReprojectionMatrixUnrecoverableSignals401AndClearsCookie(t *testing.T) {
	sessionreauth.Configure("techstack_session", false)
	mgr := mustSessionManager(t)
	store := &stubAuthStore{}
	event, recorder := sessionReprojectionTestEvent(t, mgr, authsession.Claims{
		Subject: "auth0|user-1", TenantID: "org_gone",
	})

	err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: false})(event)
	var apiErr *httpx.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("middleware error = %v, want 401 session_renewal_required", err)
	}
	details, ok := apiErr.Details.(map[string]any)
	if apiErr.Code != httpx.SessionRenewalErrorCode || !ok ||
		details["error_code"] != string(httpx.SessionRenewalErrorCode) ||
		details["reason"] != sessionreauth.ReasonCode ||
		details["renew_url"] != httpx.SessionRenewalURL {
		t.Fatalf("denial = %#v, want session_renewal_required with reason %q and renew_url", apiErr, sessionreauth.ReasonCode)
	}
	if details["retryable"] != true {
		t.Fatalf("denial retryable = %#v, want true", details["retryable"])
	}
	if _, hasGuidance := details["user_guidance"]; !hasGuidance {
		t.Fatalf("denial details = %#v, want user_guidance per FEATURE-ENTITLEMENT-UX-STANDARD", apiErr.Details)
	}
	cookie := clearedSessionCookie(t, recorder)
	if cookie == nil || cookie.Value != "" || cookie.MaxAge != -1 {
		t.Fatalf("session cookie = %+v, want cleared techstack_session", cookie)
	}
}

// The tenant-labeled counter must observe both outcomes through the
// registered hook (the visibility half of the bead's acceptance).
func TestSessionReprojectionMatrixEmitsTenantLabeledCounter(t *testing.T) {
	sessionreauth.Configure("techstack_session", false)
	type observation struct{ tenant, outcome string }
	var observed []observation
	sessionreauth.SetCounterHook(func(tenantID, outcome string) {
		observed = append(observed, observation{tenant: tenantID, outcome: outcome})
	})
	defer sessionreauth.SetCounterHook(nil)

	mgr := mustSessionManager(t)

	recoveredEvent, _ := sessionReprojectionTestEvent(t, mgr, authsession.Claims{Subject: "auth0|user-1", TenantID: "org_gone"})
	if err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: &stubAuthStore{}, defaultTenant: "default", cookieName: "techstack_session", saasMode: true})(recoveredEvent); err != nil {
		t.Fatalf("recovered case error = %v", err)
	}

	deniedEvent, _ := sessionReprojectionTestEvent(t, mgr, authsession.Claims{Subject: "auth0|user-1", TenantID: "org_gone"})
	if err := v2SessionIdentityMiddleware(&v2Boot{session: mgr, authStore: &stubAuthStore{}, defaultTenant: "default", cookieName: "techstack_session", saasMode: false})(deniedEvent); err == nil {
		t.Fatal("denied case error = nil, want 401 signal")
	}

	if len(observed) != 2 ||
		observed[0] != (observation{tenant: "org_gone", outcome: "recovered"}) ||
		observed[1] != (observation{tenant: "org_gone", outcome: "reauth_required"}) {
		t.Fatalf("counter observations = %#v, want tenant-labeled recovered + reauth_required", observed)
	}
}

// contextErrAuthStore answers GetMembership with a context error. Other
// methods keep the empty-store behavior, so an owner-tenant bootstrap would
// still be visible if the middleware treated the lookup as absence.
type contextErrAuthStore struct {
	stubAuthStore
	err error
}

func (s *contextErrAuthStore) GetMembership(context.Context, string, string) (*controlplane.Membership, error) {
	return nil, s.err
}

func sessionReprojectionReason(err error) string {
	var apiErr *httpx.APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	details, _ := apiErr.Details.(map[string]any)
	if apiErr.Code != httpx.SessionRenewalErrorCode {
		return ""
	}
	reason, _ := details["reason"].(string)
	return reason
}

// A membership read that did not answer — canceled, deadline-exceeded or a
// store fault — is not proof the membership is gone. A signed-in person keeps
// the session cookie, is never told to re-project, and the request names the
// service fault (503) instead (workspace AUTHENTICATION.md §6).
func TestFailedMembershipLookupDoesNotReprojectSession(t *testing.T) {
	t.Setenv(config.EnvReleaseChannel, "stable")
	sessionreauth.Configure("techstack_session", false)
	mgr := mustSessionManager(t)
	claims := authsession.Claims{Subject: "auth0|user-1", TenantID: "org_alive"}

	for _, lookupErr := range []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("set tenant guc: %w", context.Canceled),
		errors.New("controlplane: database is not reachable"),
	} {
		t.Run(lookupErr.Error(), func(t *testing.T) {
			sessionreauth.SetCounterHook(func(tenantID, outcome string) {
				t.Errorf("reprojection counter tenant=%s outcome=%s", tenantID, outcome)
			})
			t.Cleanup(func() { sessionreauth.SetCounterHook(nil) })

			store := &contextErrAuthStore{err: lookupErr}
			event, recorder := sessionReprojectionTestEvent(t, mgr, claims)
			event.Request.URL.Path = "/api/v2/auth/providers"

			err := v2SessionIdentityMiddleware(&v2Boot{
				session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: true,
			})(event)
			if err == nil {
				t.Fatal("middleware returned nil, want interrupted lookup rather than a signed-in continuation")
			}
			if reason := sessionReprojectionReason(err); reason == sessionreauth.ReasonCode {
				t.Fatalf("reason_code = %q, want no %s", reason, sessionreauth.ReasonCode)
			}
			var apiErr *httpx.APIError
			if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code == sessionreauth.ReasonCode {
				t.Fatalf("middleware error = %#v, want a 503 service fault, not session denial", err)
			}
			if cookie := clearedSessionCookie(t, recorder); cookie != nil {
				t.Fatalf("session cookie = %+v, want it left in place", cookie)
			}
			if id := identity.FromContext(event.Request.Context()); id != nil {
				t.Fatalf("identity = %+v, want no signed-in projection after an interrupted lookup", id)
			}
			if len(store.tenants) != 0 || len(store.memberships) != 0 {
				t.Fatalf("store mutated on interrupted lookup: tenants=%#v memberships=%#v", store.tenants, store.memberships)
			}
		})
	}

	t.Run("edge identity canceled lookup keeps cookie", func(t *testing.T) {
		sessionreauth.SetCounterHook(func(tenantID, outcome string) {
			t.Errorf("reprojection counter tenant=%s outcome=%s", tenantID, outcome)
		})
		t.Cleanup(func() { sessionreauth.SetCounterHook(nil) })

		store := &contextErrAuthStore{err: context.Canceled}
		event, recorder := sessionReprojectionTestEvent(t, mgr, claims)
		event.Request.URL.Path = "/api/v2/auth/providers"
		event.Request = event.Request.WithContext(identity.NewContext(event.Request.Context(), &identity.Identity{
			UserID: "auth0|user-1", OrgID: "org_alive",
		}))

		err := v2SessionIdentityMiddleware(&v2Boot{
			session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: true,
		})(event)
		if err == nil {
			t.Fatal("middleware returned nil, want interrupted lookup rather than a signed-in continuation")
		}
		if reason := sessionReprojectionReason(err); reason == sessionreauth.ReasonCode {
			t.Fatalf("reason_code = %q, want no %s", reason, sessionreauth.ReasonCode)
		}
		var apiErr *httpx.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusServiceUnavailable || apiErr.Code == sessionreauth.ReasonCode {
			t.Fatalf("middleware error = %#v, want a 503 service fault, not session denial", err)
		}
		if cookie := clearedSessionCookie(t, recorder); cookie != nil {
			t.Fatalf("session cookie = %+v, want it left in place", cookie)
		}
		if id := identity.FromContext(event.Request.Context()); id == nil || id.OrgID != "org_alive" {
			t.Fatalf("identity = %+v, want the edge tenant left unrebound", id)
		}
		if len(store.tenants) != 0 || len(store.memberships) != 0 {
			t.Fatalf("store mutated on interrupted lookup: tenants=%#v memberships=%#v", store.tenants, store.memberships)
		}
	})

	t.Run("missing membership still denies", func(t *testing.T) {
		store := &stubAuthStore{}
		event, recorder := sessionReprojectionTestEvent(t, mgr, authsession.Claims{
			Subject: "auth0|user-1", TenantID: "org_gone",
		})
		event.Request.URL.Path = "/api/v2/auth/providers"

		err := v2SessionIdentityMiddleware(&v2Boot{
			session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: false,
		})(event)
		var apiErr *httpx.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			t.Fatalf("middleware error = %v, want 401 session_renewal_required", err)
		}
		if reason := sessionReprojectionReason(err); reason != sessionreauth.ReasonCode {
			t.Fatalf("reason_code = %q, want %s", reason, sessionreauth.ReasonCode)
		}
		cookie := clearedSessionCookie(t, recorder)
		if cookie == nil || cookie.Value != "" || cookie.MaxAge != -1 {
			t.Fatalf("session cookie = %+v, want cleared techstack_session", cookie)
		}
	})

	t.Run("inactive membership still denies", func(t *testing.T) {
		store := &stubAuthStore{membership: &controlplane.Membership{
			TenantID: "org_alive", UserID: "auth0|user-1", RoleKey: "member", Status: "inactive",
		}}
		event, recorder := sessionReprojectionTestEvent(t, mgr, claims)
		event.Request.URL.Path = "/api/v2/auth/providers"

		err := v2SessionIdentityMiddleware(&v2Boot{
			session: mgr, authStore: store, defaultTenant: "default", cookieName: "techstack_session", saasMode: false,
		})(event)
		var apiErr *httpx.APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized {
			t.Fatalf("middleware error = %v, want 401 session_renewal_required", err)
		}
		if reason := sessionReprojectionReason(err); reason != sessionreauth.ReasonCode {
			t.Fatalf("reason_code = %q, want %s", reason, sessionreauth.ReasonCode)
		}
		cookie := clearedSessionCookie(t, recorder)
		if cookie == nil || cookie.Value != "" || cookie.MaxAge != -1 {
			t.Fatalf("session cookie = %+v, want cleared techstack_session", cookie)
		}
	})
}
