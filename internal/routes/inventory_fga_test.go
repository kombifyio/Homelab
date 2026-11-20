package routes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	commonedgeauth "github.com/kombifyio/homelab/internal/gocommon/edgeauth"
	"github.com/kombifyio/homelab/internal/gocommon/servicecall"
	"github.com/kombifyio/homelab/pkg/httpx"

	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/identity"
	"github.com/kombifyio/homelab/pkg/middleware"
)

type fakeInventoryRelationshipChecker struct {
	allowed  bool
	err      error
	calls    int
	user     string
	relation string
	object   string
}

type countingInventoryReadStore struct {
	controlplane.InventoryReadStore
	reads int
}

func (s *countingInventoryReadStore) GetInventoryServer(ctx context.Context, scope controlplane.InventoryReadScope, serverID string) (*controlplane.ServerRuntime, error) {
	s.reads++
	return s.InventoryReadStore.GetInventoryServer(ctx, scope, serverID)
}

func (s *countingInventoryReadStore) GetInventoryStack(ctx context.Context, scope controlplane.InventoryReadScope, stackID string) (*controlplane.Stack, error) {
	s.reads++
	return s.InventoryReadStore.GetInventoryStack(ctx, scope, stackID)
}

func (s *countingInventoryReadStore) ListInventoryServers(ctx context.Context, scope controlplane.InventoryReadScope, page controlplane.InventoryPageRequest) (controlplane.InventoryServerPage, error) {
	s.reads++
	return s.InventoryReadStore.ListInventoryServers(ctx, scope, page)
}

func (s *countingInventoryReadStore) ListInventoryServices(ctx context.Context, scope controlplane.InventoryReadScope, serverID string, page controlplane.InventoryPageRequest) (controlplane.InventoryServicePage, error) {
	s.reads++
	return s.InventoryReadStore.ListInventoryServices(ctx, scope, serverID, page)
}

func (f *fakeInventoryRelationshipChecker) Check(_ context.Context, user, relation, object string) (bool, error) {
	f.calls++
	f.user, f.relation, f.object = user, relation, object
	return f.allowed, f.err
}

func TestInventoryFGAPolicyRequiresEntitlementAndRelationship(t *testing.T) {
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	policy := NewInventoryFGAPolicy(checker)
	authorization := InventoryAuthorization{
		TenantID: "tenant-1", SubjectID: "owner-1",
		ResourceType: "server", ResourceID: "server-1", Action: InventoryActionRead,
	}
	if _, err := policy.AuthorizeInventory(t.Context(), authorization); !errors.Is(err, ErrInventoryAccessDenied) {
		t.Fatalf("missing entitlement error = %v, want access denied", err)
	}
	if checker.calls != 0 {
		t.Fatalf("FGA called before entitlement gate: %d", checker.calls)
	}
	featureFlagOnly := commonedgeauth.FlagsToContext(t.Context(), commonedgeauth.FlagSet{Flags: map[string]bool{InventoryEntitlementRead: true}})
	if _, err := policy.AuthorizeInventory(featureFlagOnly, authorization); !errors.Is(err, ErrInventoryAccessDenied) {
		t.Fatalf("feature flag was treated as signed entitlement: %v", err)
	}
	if checker.calls != 0 {
		t.Fatalf("FGA called for feature-flag-only request: %d", checker.calls)
	}

	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)
	decision, err := policy.AuthorizeInventory(ctx, authorization)
	if err != nil {
		t.Fatalf("authorized read: %v", err)
	}
	targetType, targetID := decision.ReadScope.Target()
	if decision.ReadScope.IsOwnerScoped() || decision.ReadScope.TenantID() != "tenant-1" || targetType != "server" || targetID != "server-1" {
		t.Fatalf("exact-object read scope = %#v", decision.ReadScope)
	}
	if checker.user != "user:owner-1" || checker.relation != inventoryFGARelationAccessor || checker.object != "surface:tenant-1/inventory/servers" {
		t.Fatalf("FGA tuple = (%q,%q,%q)", checker.user, checker.relation, checker.object)
	}

	wildcardCtx := middleware.WithSignedEntitlements(t.Context(), "*")
	if _, err := policy.AuthorizeInventory(wildcardCtx, authorization); err != nil {
		t.Fatalf("canonical signed wildcard read: %v", err)
	}
	if checker.calls != 2 {
		t.Fatalf("FGA wildcard read calls = %d, want 2 total", checker.calls)
	}

	authorization.Action = InventoryActionOperate
	if _, err := policy.AuthorizeInventory(ctx, authorization); !errors.Is(err, ErrInventoryAccessDenied) {
		t.Fatalf("read entitlement authorized operate: %v", err)
	}
	if checker.calls != 2 {
		t.Fatalf("FGA called for entitlement denial: %d", checker.calls)
	}
	for _, action := range []InventoryAction{InventoryActionWrite, InventoryActionProvision, InventoryActionOperate, InventoryActionTransfer, InventoryActionAdmin, InventoryActionRILRead} {
		restricted := authorization
		restricted.Action = action
		if _, err := policy.AuthorizeInventory(middleware.WithSignedEntitlements(t.Context(), "mcp.customer.access"), restricted); !errors.Is(err, ErrInventoryAccessDenied) {
			t.Fatalf("customer inventory baseline authorized %s: %v", action, err)
		}
	}

	operateCtx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementOperate)
	if _, err := policy.AuthorizeInventory(operateCtx, authorization); err != nil {
		t.Fatalf("authorized operate: %v", err)
	}
	if checker.calls != 3 {
		t.Fatalf("FGA operate calls = %d, want 3 total", checker.calls)
	}
	if checker.user != "user:owner-1" || checker.relation != inventoryFGARelationCaller || checker.object != "server:tenant-1/server-1" {
		t.Fatalf("operate FGA tuple = (%q,%q,%q)", checker.user, checker.relation, checker.object)
	}
}

func TestInventoryFGAPolicyKeepsRILSummaryEntitlementRouteScoped(t *testing.T) {
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	policy := NewInventoryFGAPolicy(checker)
	authorization := InventoryAuthorization{
		TenantID: "tenant-1", SubjectID: "owner-1",
		ResourceType: controlplane.InventoryReadTargetServerCollection,
		Action:       InventoryActionRILRead,
	}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRILRead)
	decision, err := policy.AuthorizeInventory(ctx, authorization)
	if err != nil {
		t.Fatalf("authorized RIL summary read: %v", err)
	}
	if !decision.ReadScope.IsOwnerScoped() || checker.relation != inventoryFGARelationAccessor || checker.object != "surface:tenant-1/inventory/servers" {
		t.Fatalf("RIL summary decision = scope:%#v relation:%q object:%q", decision.ReadScope, checker.relation, checker.object)
	}

	authorization.Action = InventoryActionRead
	if _, err := policy.AuthorizeInventory(ctx, authorization); !errors.Is(err, ErrInventoryAccessDenied) {
		t.Fatalf("RIL entitlement widened the general inventory API: %v", err)
	}
}

func TestInventoryFGAObjectEncodesPersonalTenantTypeSeparators(t *testing.T) {
	authorization := InventoryAuthorization{
		TenantID: "usr:auth0|fixture", SubjectID: "auth0|fixture",
		ResourceType: controlplane.InventoryReadTargetServerCollection,
		Action:       InventoryActionRILRead,
	}
	object, err := inventoryFGAObject(authorization)
	if err != nil {
		t.Fatal(err)
	}
	if object != "surface:b64_dXNyOmF1dGgwfGZpeHR1cmU/inventory/servers" {
		t.Fatalf("personal tenant object = %q", object)
	}

	authorization.TenantID = "tenant-1"
	object, err = inventoryFGAObject(authorization)
	if err != nil || object != "surface:tenant-1/inventory/servers" {
		t.Fatalf("existing organization object changed: %q, %v", object, err)
	}
}

func TestInventoryFGAPolicyRejectsRelationsMissingFromThePinnedModel(t *testing.T) {
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	policy := NewInventoryFGAPolicy(checker)
	authorization := InventoryAuthorization{
		TenantID: "tenant-1", SubjectID: "admin-1",
		ResourceType: "server_collection", Action: InventoryActionAdmin,
	}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementAdmin)
	if _, err := policy.AuthorizeInventory(ctx, authorization); !errors.Is(err, ErrInventoryAccessDenied) {
		t.Fatalf("unmodeled admin relation error = %v, want access denied", err)
	}
	if checker.calls != 0 {
		t.Fatalf("unmodeled admin relation reached FGA %d times", checker.calls)
	}
}

func TestInventoryApplicationUsesFGAExactObjectScopeForCrossOwnerRead(t *testing.T) {
	store := controlplane.NewMemoryStore()
	if _, err := store.UpsertServerRuntime(t.Context(), controlplane.ServerRuntime{
		ID: "server-owner-2", TenantID: "tenant-1", OwnerSubjectID: "owner-2", Name: "Delegated",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertServerRuntime(t.Context(), controlplane.ServerRuntime{
		ID: "server-other-tenant", TenantID: "tenant-2", OwnerSubjectID: "reader-1", Name: "Other tenant",
	}); err != nil {
		t.Fatal(err)
	}
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	app := &inventoryApplication{read: store, policy: NewInventoryFGAPolicy(checker), now: time.Now}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)

	result, err := app.serverHealth(ctx, inventoryScope{tenantID: "tenant-1", ownerID: "reader-1"}, "server-owner-2")
	if err != nil {
		t.Fatalf("cross-owner FGA read: %v", err)
	}
	if result.ServerID != "server-owner-2" {
		t.Fatalf("server health = %#v", result)
	}
	if checker.object != "surface:tenant-1/inventory/servers" || checker.relation != inventoryFGARelationAccessor {
		t.Fatalf("FGA tuple = (%q,%q)", checker.relation, checker.object)
	}

	if _, err := app.serverHealth(ctx, inventoryScope{tenantID: "tenant-1", ownerID: "reader-1"}, "server-other-tenant"); err == nil {
		t.Fatal("exact-object scope crossed the authenticated tenant")
	}
}

func TestInventoryAdminMembershipDoesNotImplicitlyWidenRESTOrMCPScope(t *testing.T) {
	store := controlplane.NewMemoryStore()
	for _, server := range []controlplane.ServerRuntime{
		{ID: "server-admin-1", TenantID: "tenant-1", OwnerSubjectID: "admin-1"},
		{ID: "server-owner-2", TenantID: "tenant-1", OwnerSubjectID: "owner-2"},
		{ID: "server-other-tenant", TenantID: "tenant-2", OwnerSubjectID: "owner-1"},
	} {
		if _, err := store.UpsertServerRuntime(t.Context(), server); err != nil {
			t.Fatal(err)
		}
	}
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	app := &inventoryApplication{read: store, policy: NewInventoryFGAPolicy(checker), now: time.Now}
	h := inventoryHandlers{app: app, version: "test"}
	event, recorder := registryRouteStoreTestEvent(http.MethodGet, "/api/v1/inventory/servers?limit=10", "admin-1", "tenant-1", nil)
	ctx := identity.NewContext(event.Request.Context(), &identity.Identity{
		UserID: "admin-1", OrgID: "tenant-1", Roles: []string{"admin"},
	})
	event.Request = event.Request.WithContext(middleware.WithSignedEntitlements(ctx, "*"))

	if err := h.httpListServers(event); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin membership read status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"server-admin-1"`) || strings.Contains(body, `"server-owner-2"`) || strings.Contains(body, `"server-other-tenant"`) {
		t.Fatalf("admin membership implicitly widened inventory scope: %s", body)
	}
	if checker.calls != 1 || checker.relation != inventoryFGARelationAccessor ||
		checker.object != "surface:tenant-1/inventory/servers" {
		t.Fatalf("admin membership FGA decision = calls:%d relation:%q object:%q", checker.calls, checker.relation, checker.object)
	}

	mcpEvent, mcpRecorder := registryRouteStoreTestEvent(http.MethodPost, "/api/v1/mcp", "admin-1", "tenant-1", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	})
	mcpContext := identity.NewContext(mcpEvent.Request.Context(), &identity.Identity{
		UserID: "admin-1", OrgID: "tenant-1", Roles: []string{"admin"},
	})
	mcpEvent.Request = mcpEvent.Request.WithContext(middleware.WithSignedEntitlements(mcpContext, "*"))
	if err := h.handleMCP(mcpEvent); err != nil {
		t.Fatal(err)
	}
	if mcpRecorder.Code != http.StatusOK || !strings.Contains(mcpRecorder.Body.String(), `"tools"`) {
		t.Fatalf("admin membership MCP tools/list = %d %s", mcpRecorder.Code, mcpRecorder.Body.String())
	}
	if checker.calls != 2 || checker.relation != inventoryFGARelationAccessor ||
		checker.object != "surface:tenant-1/inventory/tools" {
		t.Fatalf("admin membership MCP FGA decision = calls:%d relation:%q object:%q", checker.calls, checker.relation, checker.object)
	}
}

func TestInventoryApplicationRegularCollectionKeepsOwnerPredicateAfterFGAAllow(t *testing.T) {
	store := controlplane.NewMemoryStore()
	for _, server := range []controlplane.ServerRuntime{
		{ID: "server-owner-1", TenantID: "tenant-1", OwnerSubjectID: "owner-1"},
		{ID: "server-owner-2", TenantID: "tenant-1", OwnerSubjectID: "owner-2"},
	} {
		if _, err := store.UpsertServerRuntime(t.Context(), server); err != nil {
			t.Fatal(err)
		}
	}
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	app := &inventoryApplication{read: store, policy: NewInventoryFGAPolicy(checker), now: time.Now}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)

	result, err := app.listServers(ctx, inventoryScope{tenantID: "tenant-1", ownerID: "owner-1"}, inventoryPageOptions{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Servers) != 1 || result.Servers[0].ID != "server-owner-1" {
		t.Fatalf("regular FGA collection widened owner predicate: %#v", result.Servers)
	}
}

func TestInventoryRESTAndMCPShareFGAExactObjectApplicationScope(t *testing.T) {
	store := controlplane.NewMemoryStore()
	if _, err := store.UpsertServerRuntime(t.Context(), controlplane.ServerRuntime{
		ID: "delegated-server", TenantID: "tenant-1", OwnerSubjectID: "owner-2", Name: "Delegated",
	}); err != nil {
		t.Fatal(err)
	}
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	h := inventoryHandlers{app: &inventoryApplication{read: store, policy: NewInventoryFGAPolicy(checker), now: time.Now}, version: "test"}

	restEvent, restRecorder := registryRouteStoreTestEvent(http.MethodGet, "/api/v1/inventory/servers/delegated-server/health", "reader-1", "tenant-1", nil)
	restEvent.Request.SetPathValue("serverId", "delegated-server")
	restEvent.Request = restEvent.Request.WithContext(middleware.WithSignedEntitlements(restEvent.Request.Context(), InventoryEntitlementRead))
	if err := h.httpServerHealth(restEvent); err != nil {
		t.Fatal(err)
	}
	if restRecorder.Code != http.StatusOK || !strings.Contains(restRecorder.Body.String(), `"server_id":"delegated-server"`) {
		t.Fatalf("REST delegated read = %d %s", restRecorder.Code, restRecorder.Body.String())
	}

	mcpEvent, mcpRecorder := registryRouteStoreTestEvent(http.MethodPost, "/api/v1/mcp", "reader-1", "tenant-1", map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{"name": "server_health", "arguments": map[string]any{"server_id": "delegated-server"}},
	})
	mcpEvent.Request = mcpEvent.Request.WithContext(middleware.WithSignedEntitlements(mcpEvent.Request.Context(), InventoryEntitlementRead))
	if err := h.handleMCP(mcpEvent); err != nil {
		t.Fatal(err)
	}
	if mcpRecorder.Code != http.StatusOK || !strings.Contains(mcpRecorder.Body.String(), `"server_id":"delegated-server"`) || strings.Contains(mcpRecorder.Body.String(), `"isError":true`) {
		t.Fatalf("MCP delegated read = %d %s", mcpRecorder.Code, mcpRecorder.Body.String())
	}
	if checker.calls != 2 {
		t.Fatalf("shared FGA policy calls = %d, want one per transport request", checker.calls)
	}
}

func TestInventoryApplicationFGADenialHappensBeforeStoreRead(t *testing.T) {
	read := &countingInventoryReadStore{InventoryReadStore: controlplane.NewMemoryStore()}
	checker := &fakeInventoryRelationshipChecker{allowed: false}
	app := &inventoryApplication{read: read, policy: NewInventoryFGAPolicy(checker), now: time.Now}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)

	if _, err := app.listServers(ctx, inventoryScope{tenantID: "tenant-1", ownerID: "reader-1"}, inventoryPageOptions{Limit: 10}); err == nil {
		t.Fatal("FGA denial returned no error")
	}
	if checker.calls != 1 || read.reads != 0 {
		t.Fatalf("denial calls = fga:%d store:%d, want FGA once and zero reads", checker.calls, read.reads)
	}
}

func TestInventoryApplicationRejectsMalformedPolicyDecisionBeforeStoreRead(t *testing.T) {
	otherTenant, err := controlplane.NewTenantInventoryCollectionReadScope("tenant-2", controlplane.InventoryReadTargetServerCollection)
	if err != nil {
		t.Fatal(err)
	}
	otherOwner, err := controlplane.NewOwnerInventoryReadScope("tenant-1", "owner-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		decision InventoryDecision
	}{
		{name: "zero decision"},
		{name: "wrong tenant", decision: InventoryDecision{ReadScope: otherTenant}},
		{name: "forged owner", decision: InventoryDecision{ReadScope: otherOwner}},
	} {
		t.Run(test.name, func(t *testing.T) {
			read := &countingInventoryReadStore{InventoryReadStore: controlplane.NewMemoryStore()}
			app := &inventoryApplication{
				read: read,
				policy: InventoryPolicyFunc(func(context.Context, InventoryAuthorization) (InventoryDecision, error) {
					return test.decision, nil
				}),
				now: time.Now,
			}

			_, err := app.listServers(t.Context(), inventoryScope{tenantID: "tenant-1", ownerID: "reader-1"}, inventoryPageOptions{Limit: 10})
			var inventoryErr *inventoryError
			if !errors.As(err, &inventoryErr) || inventoryErr.status != http.StatusServiceUnavailable {
				t.Fatalf("malformed policy decision error = %#v, want fail-closed unavailable", err)
			}
			if read.reads != 0 {
				t.Fatalf("malformed decision reached store %d times", read.reads)
			}
		})
	}
}

func TestInventoryFGAPolicyFailsUnavailableWithoutCheckerOrOnError(t *testing.T) {
	authorization := InventoryAuthorization{
		TenantID: "tenant-1", SubjectID: "owner-1",
		ResourceType: "service_collection", Action: InventoryActionRead,
	}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)
	if _, err := NewInventoryFGAPolicy(nil).AuthorizeInventory(ctx, authorization); !errors.Is(err, errInventoryPolicyUnavailable) {
		t.Fatalf("nil checker error = %v, want unavailable", err)
	}
	checker := &fakeInventoryRelationshipChecker{err: errors.New("backend unavailable")}
	if _, err := NewInventoryFGAPolicy(checker).AuthorizeInventory(ctx, authorization); !errors.Is(err, errInventoryPolicyUnavailable) {
		t.Fatalf("backend error = %v, want unavailable", err)
	}
}

func TestInventoryFGAPolicyMapsMCPToolCatalogToTenantSurface(t *testing.T) {
	checker := &fakeInventoryRelationshipChecker{allowed: true}
	policy := NewInventoryFGAPolicy(checker)
	authorization := InventoryAuthorization{
		TenantID: "tenant-1", SubjectID: "owner-1",
		ResourceType: "inventory_tools", Action: InventoryActionRead,
	}
	ctx := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementRead)
	if _, err := policy.AuthorizeInventory(ctx, authorization); err != nil {
		t.Fatalf("authorized tool catalog: %v", err)
	}
	if checker.relation != inventoryFGARelationAccessor || checker.object != "surface:tenant-1/inventory/tools" {
		t.Fatalf("tool catalog FGA tuple = (%q,%q)", checker.relation, checker.object)
	}
}

func TestInventoryFGAPolicyUnavailableUsesSameFailClosedRESTAndMCPApplication(t *testing.T) {
	checker := &fakeInventoryRelationshipChecker{err: errors.New("FGA backend unavailable")}
	h := inventoryHandlers{
		app: &inventoryApplication{
			read: controlplane.NewMemoryStore(), policy: NewInventoryFGAPolicy(checker), now: time.Now,
		},
		version: "test",
	}

	restEvent, restRecorder := registryRouteStoreTestEvent(http.MethodGet, "/api/v1/inventory/servers", "owner-1", "tenant-1", nil)
	restEvent.Request = restEvent.Request.WithContext(middleware.WithSignedEntitlements(restEvent.Request.Context(), InventoryEntitlementRead))
	if err := h.httpListServers(restEvent); err != nil {
		t.Fatalf("REST inventory request: %v", err)
	}
	if restRecorder.Code != http.StatusServiceUnavailable || !strings.Contains(restRecorder.Body.String(), `"reason_code":"inventory_policy_unavailable"`) {
		t.Fatalf("REST response = %d %s, want fail-closed 503", restRecorder.Code, restRecorder.Body.String())
	}

	mcpEvent, mcpRecorder := registryRouteStoreTestEvent(http.MethodPost, "/api/v1/mcp", "owner-1", "tenant-1", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_servers", "arguments": map[string]any{}},
	})
	mcpEvent.Request = mcpEvent.Request.WithContext(middleware.WithSignedEntitlements(mcpEvent.Request.Context(), InventoryEntitlementRead))
	if err := h.handleMCP(mcpEvent); err != nil {
		t.Fatalf("MCP inventory request: %v", err)
	}
	if mcpRecorder.Code != http.StatusOK || !strings.Contains(mcpRecorder.Body.String(), `"isError":true`) ||
		!strings.Contains(mcpRecorder.Body.String(), `"status":503`) ||
		!strings.Contains(mcpRecorder.Body.String(), `"reason_code":"inventory_policy_unavailable"`) {
		t.Fatalf("MCP response = %d %s, want fail-closed tool error", mcpRecorder.Code, mcpRecorder.Body.String())
	}
}

// An owner operates their own server on the signed grant alone: nothing
// provisions a per-server FGA grant for the owner. Another subject, a missing
// entitlement and a household member still need the grant.
func TestInventoryOwnerOperatesOwnServerWithoutAServerGrant(t *testing.T) {
	store := controlplane.NewMemoryStore()
	if _, err := store.UpsertServerRuntime(t.Context(), controlplane.ServerRuntime{
		ID: "server-1", TenantID: "tenant-1", StackID: "stack-1", OwnerSubjectID: "owner-1", WorkerID: "worker-1",
		Name: "Node", LifecycleState: "active", ConnectionState: "connected", HealthState: "healthy",
	}); err != nil {
		t.Fatal(err)
	}
	checker := &fakeInventoryRelationshipChecker{allowed: false}
	policy := NewInventoryFGAPolicy(checker)
	signed := middleware.WithSignedEntitlements(t.Context(), InventoryEntitlementOperate)

	if err := authorizeOwnerServerOperate(signed, policy, store, inventoryScope{tenantID: "tenant-1", ownerID: "owner-1"}, "server-1"); err != nil {
		t.Fatalf("the owner was denied without a server grant: %v", err)
	}
	if err := authorizeOwnerServerOperate(signed, policy, store, inventoryScope{tenantID: "tenant-1", ownerID: "owner-2"}, "server-1"); !isInventoryDenied(err) {
		t.Fatalf("another subject got %v", err)
	}
	if err := authorizeOwnerServerOperate(t.Context(), policy, store, inventoryScope{tenantID: "tenant-1", ownerID: "owner-1"}, "server-1"); !isInventoryDenied(err) {
		t.Fatalf("the owner without the signed entitlement got %v", err)
	}
	household := inventoryScope{tenantID: householdOwnerTenantID("owner-1"), ownerID: "member-1", householdOwnerID: "owner-1"}
	if err := authorizeOwnerServerOperate(signed, policy, store, household, "server-1"); !isInventoryDenied(err) {
		t.Fatalf("a household member without a grant got %v", err)
	}
}

func isInventoryDenied(err error) bool {
	var inventoryErr *inventoryError
	return errors.As(err, &inventoryErr) && inventoryErr.reasonCode == "inventory_access_denied"
}

// The signed Cloud read projects owner intent independently of runtime inventory.
func TestRuntimeSummaryProjectsSavedPlanAfterFailedJob(t *testing.T) {
	ctx := t.Context()
	store := controlplane.NewMemoryStore()
	intent := map[string]any{"wizard": map[string]any{"goals": []any{"photos", "media", "files", "documents"}, "last_run_id": "run-1", "use_case_settings": map[string]any{"photos": map[string]any{"machine-learning": false}}, "use_case_setup_answers": map[string]any{"photos": map[string]any{"library-volume": []any{"dedicated-disk"}, "extras": []any{"machine-learning", "public-sharing"}}, "media": map[string]any{"extras-2": []any{"music-server"}}}}}
	if _, err := store.CreateHomelab(ctx, controlplane.CreateHomelabRequest{ID: "home-1", TenantID: "tenant-1", OwnerSubjectID: "owner-1", Name: "Home", Intent: intent}); err != nil {
		t.Fatal(err)
	}
	spec := map[string]any{"apiVersion": "stackkit/v2alpha1", "kind": "StackSpec", "kit": map[string]any{"slug": "basement-kit"}, "workloads": map[string]any{"photos": map[string]any{"enabled": true, "alternative": "immich-lite", "params": map[string]any{"token": "never-return"}}, "media": map[string]any{"alternative": "jellyfin"}, "files": map[string]any{"alternative": "cloudreve"}, "documents": map[string]any{"enabled": false, "alternative": "paperless-ngx"}}}
	foreignSpec := map[string]any{"apiVersion": "stackkit/v2alpha1", "kind": "StackSpec", "kit": map[string]any{"slug": "basement-kit"}, "workloads": map[string]any{"documents": map[string]any{"alternative": "paperless-ngx"}}}
	for _, req := range []controlplane.CreateStackRequest{{ID: "owned", TenantID: "tenant-1", OwnerSubjectID: "owner-1", HomelabID: "home-1", Name: "owned", Config: map[string]any{"stack_spec_v2": spec}}, {ID: "legacy", TenantID: "tenant-1", OwnerSubjectID: "owner-1", Name: "legacy"}, {ID: "foreign", TenantID: "tenant-1", OwnerSubjectID: "other", HomelabID: "home-1", Name: "foreign", Config: map[string]any{"stack_spec_v2": foreignSpec}}} {
		if _, err := store.CreateStack(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.UpsertWizardRun(ctx, controlplane.WizardRun{ID: "run-1", RequestSHA256: "fixture", RunKind: "create", RequestedRunKind: "create", Status: "failed", TenantID: "tenant-1", OwnerSubjectID: "owner-1", HomelabID: "home-1", StackID: "owned", JobID: "job-1", Intent: map[string]any{"intent": map[string]any{"goals": []any{"photos", "media", "files", "documents"}}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertJob(ctx, controlplane.UpsertJobRequest{ID: "job-1", TenantID: "tenant-1", StackID: "owned", State: "failed", Step: "stackkit_rollout", Result: map[string]any{"reason_code": "provision_failed", "retryable": true, "user_guidance": map[string]any{"body": "host db.internal /var/log leak"}, "support_context": map[string]any{"failed_step": "other", "stackkit_reason_code": "acme_rate_limited", "target_id": "db.internal"}}}); err != nil {
		t.Fatal(err)
	}
	const secret = "runtime-summary-fixture-secret"
	router := httpx.NewRouter()
	RegisterInventoryRoutes(router, InventoryRouteConfig{RuntimeSummaryContext: func(ctx context.Context, tenant, owner string) (context.Context, string, error) {
		return ctx, tenant, nil
	}, ReadStore: store, Homelabs: store, WizardRuns: store, Jobs: store, OwnerStackSpecs: store, ServiceAuthSecret: secret, Now: time.Now})
	token, err := servicecall.IssueToken(servicecall.Config{ServiceName: "cloud", Secret: secret, TokenTTL: time.Minute}, "homelab", &servicecall.OnBehalfOf{Sub: "owner-1", OrgID: "tenant-1"}, "")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/internal/runtime/summary", nil)
	req.Header.Set(servicecall.HeaderServiceAuth, token)
	rec := httptest.NewRecorder()
	router.BuildMux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var result struct {
		Configured []configuredUseCase `json:"configured_use_cases"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	items := map[string]configuredUseCase{}
	for _, item := range result.Configured {
		items[item.Goal] = item
	}
	for _, id := range []string{"photos", "media", "files", "documents"} {
		if items[id].Status != "failed" {
			t.Errorf("%s status = %q", id, items[id].Status)
		}
	}
	if items["photos"].Settings["library-volume"] != "dedicated-disk" || items["photos"].Settings["machine-learning"] != false || items["photos"].Settings["public-sharing"] != true || items["media"].Settings["music-server"] != true {
		t.Errorf("settings = %#v / %#v", items["photos"].Settings, items["media"].Settings)
	}
	for goal, want := range map[string]string{"photos": "immich-lite", "media": "jellyfin", "files": "cloudreve"} {
		apps := items[goal].PlannedApps
		if len(apps) != 1 || apps[0].ID != want {
			t.Errorf("%s apps = %#v", goal, apps)
		}
	}
	if f := items["photos"].Failure; f == nil || f.Code != "provision_failed" || f.DetailCode != "acme_rate_limited" || f.Step != "stackkit_rollout" || f.OperationID != "job-1" || !f.Retryable || f.OccurredAt == "" || strings.Contains(rec.Body.String(), "db.internal") {
		t.Errorf("failure = %#v in %s", f, rec.Body.String())
	}
	if len(items["documents"].PlannedApps) != 0 || strings.Contains(rec.Body.String(), "never-return") {
		t.Errorf("unexpected app or private params: %s", rec.Body.String())
	}
}
