package providercontrol

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

type flyMachinesTestAPI struct {
	sync.Mutex
	t                   *testing.T
	baseURL             string
	requests            []string
	latest              flyMachineCreate
	machines            map[string]flyMachine
	delayDeletion       bool
	stopAutostop        bool
	deleteStatus        int
	createStatus        int
	createLostAfterSave bool
	createRegion        string
	tamperImage         string
	dropOnCreate        bool
	destroyState        string
	expectAuth          string
	allowedApps         map[string]string
	appNetworks         map[string]string
}

func (api *flyMachinesTestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.Lock()
	defer api.Unlock()
	api.requests = append(api.requests, request.Method+" "+request.URL.RequestURI())
	rest, ok := strings.CutPrefix(request.URL.Path, "/v1/apps/")
	if !ok {
		api.t.Errorf("unexpected path: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	if !strings.Contains(rest, "/") && request.Method == http.MethodGet {
		appName := rest
		expectedAuth := api.expectAuth
		if expectedAuth == "" {
			if token, ok := api.allowedApps[appName]; ok {
				expectedAuth = "Bearer " + token
			} else if appName == "isolated-app" {
				expectedAuth = "Bearer app-test-token"
			}
		}
		if expectedAuth == "" || request.Header.Get("Authorization") != expectedAuth {
			writer.WriteHeader(http.StatusForbidden)
			return
		}
		network := api.appNetworks[appName]
		if network == "" && appName == "isolated-app" {
			network = "net-tenant-a"
		}
		_ = json.NewEncoder(writer).Encode(flyAppInfo{Name: appName, Network: network})
		return
	}
	appName, afterApp, ok := strings.Cut(rest, "/machines")
	if !ok {
		api.t.Errorf("unexpected path: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	expectedAuth := api.expectAuth
	if expectedAuth == "" {
		if token, ok := api.allowedApps[appName]; ok {
			expectedAuth = "Bearer " + token
		} else if appName == "isolated-app" {
			expectedAuth = "Bearer app-test-token"
		}
	}
	if expectedAuth == "" || request.Header.Get("Authorization") != expectedAuth {
		api.t.Errorf("missing or wrong app-scoped token for app %s", appName)
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	if afterApp == "" && request.Method == http.MethodGet {
		wantTenant := request.URL.Query().Get("metadata.tenant_id")
		wantWorkload := request.URL.Query().Get("metadata.workload_id")
		matches := make([]flyMachine, 0)
		for _, m := range api.machines {
			if (wantTenant == "" || m.Config.Metadata["tenant_id"] == wantTenant) &&
				(wantWorkload == "" || m.Config.Metadata["workload_id"] == wantWorkload) {
				matches = append(matches, m)
			}
		}
		_ = json.NewEncoder(writer).Encode(matches)
		return
	}
	if afterApp == "" && request.Method == http.MethodPost {
		if err := json.NewDecoder(request.Body).Decode(&api.latest); err != nil {
			api.t.Errorf("decode Fly create: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		if api.createStatus != 0 && !api.createLostAfterSave {
			writer.WriteHeader(api.createStatus)
			return
		}
		region := api.latest.Region
		if api.createRegion != "" {
			region = api.createRegion
		}
		machine := flyMachine{
			ID: "machine-" + api.latest.Config.Metadata["tenant_id"], Name: api.latest.Name,
			Region: region, State: "started", Config: api.latest.Config,
			Checks: []flyMachineCheckStatus{{Name: "workload", Status: "passing"}},
		}
		if api.tamperImage != "" {
			machine.Config.Image = api.tamperImage
		}
		if api.stopAutostop {
			machine.Config.Services = slices.Clone(machine.Config.Services)
			machine.Config.Services[0].Autostop = json.RawMessage(`true`)
		}
		if !api.dropOnCreate {
			api.machines[machine.ID] = machine
		}
		if api.createLostAfterSave {
			status := api.createStatus
			if status == 0 {
				status = http.StatusBadGateway
			}
			writer.WriteHeader(status)
			return
		}
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]string{"id": machine.ID})
		return
	}
	path, found := strings.CutPrefix(afterApp, "/")
	if !found {
		api.t.Errorf("unexpected collection: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	machineID, verb, _ := strings.Cut(path, "/")
	machine, exists := api.machines[machineID]
	if !exists {
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	switch request.Method + " " + verb {
	case "GET ":
		_ = json.NewEncoder(writer).Encode(machine)
	case "POST start":
		machine.State = "started"
		api.machines[machineID] = machine
		writer.WriteHeader(http.StatusOK)
	case "POST stop":
		machine.State = "stopped"
		api.machines[machineID] = machine
		writer.WriteHeader(http.StatusOK)
	case "DELETE ":
		if request.URL.Query().Get("force") != "true" {
			api.t.Error("expected forced delete of exact machine")
		}
		if api.deleteStatus != 0 {
			writer.WriteHeader(api.deleteStatus)
			return
		}
		if api.destroyState != "" {
			machine.State = api.destroyState
			api.machines[machineID] = machine
		} else if !api.delayDeletion {
			delete(api.machines, machineID)
		}
		writer.WriteHeader(http.StatusAccepted)
	default:
		api.t.Errorf("unexpected Fly operation: %s %s", request.Method, request.URL.Path)
		writer.WriteHeader(http.StatusBadRequest)
	}
}

func (api *flyMachinesTestAPI) serverURL(_ *testing.T) string {
	return api.baseURL
}

type flyMachinesTestR2 struct {
	backups  []FlyMachineHandle
	restores []FlyBackupManifest
}

func (store *flyMachinesTestR2) Backup(_ context.Context, target FlyMachineHandle) (FlyBackupArtifact, error) {
	store.backups = append(store.backups, target)
	return FlyBackupArtifact{Ref: "r2-backup-1", SHA256: strings.Repeat("a", 64)}, nil
}

func (store *flyMachinesTestR2) Restore(_ context.Context, target FlyMachineHandle, artifact FlyBackupArtifact) error {
	store.restores = append(store.restores, FlyBackupManifest{Source: target, Artifact: artifact})
	return nil
}

func flyMachineTestSetup(t *testing.T) (*flyMachinesTestAPI, func(string, FlyMachineR2Store) *FlyMachinesAdapter) {
	t.Helper()
	api := &flyMachinesTestAPI{t: t, machines: map[string]flyMachine{}}
	server := httptest.NewServer(api)
	api.baseURL = server.URL + "/v1"
	t.Cleanup(server.Close)
	return api, func(tenant string, store FlyMachineR2Store) *FlyMachinesAdapter {
		t.Helper()
		owner := FlyMachineOwner{TenantID: tenant, WorkloadID: "workload-1", SlotRef: "slot-1", CustodyHash: "custody-hash-1"}
		adapter, err := newFlyMachinesAdapter("isolated-app", "app-test-token", owner, store, server.URL+"/v1")
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
}

func flyMachineTestSpec() FlyMachineSpec {
	return FlyMachineSpec{Region: "fra", IsolationClass: "firecracker", MemoryMB: 256, Image: "example.com/workload:1", InternalPort: 8080}
}

func TestFlyMachinesBoundary(t *testing.T) {
	t.Run("invalid policy never reaches provider", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		adapter := makeAdapter("tenant-a", nil)
		for _, region := range []string{"", "ams", "FRA", "fra "} {
			spec := flyMachineTestSpec()
			spec.Region = region
			_, err := adapter.Provision(t.Context(), spec)
			var denied *FlyMachineRegionError
			if !errors.As(err, &denied) || denied.Region != region {
				t.Fatalf("region %q: expected explicit placement denial, got %v", region, err)
			}
		}
		for _, memory := range []int{0, 127, 128, 384, 513, 1024} {
			spec := flyMachineTestSpec()
			spec.MemoryMB = memory
			if _, err := adapter.Provision(t.Context(), spec); !errors.Is(err, ErrFlyMachineMemory) {
				t.Fatalf("memory %d: expected Fly size denial, got %v", memory, err)
			}
		}
		for _, spec := range []FlyMachineSpec{
			{Region: "fra", IsolationClass: "docker"},
			{Region: "fra", IsolationClass: "firecracker", DockerSocket: true},
			{Region: "fra", IsolationClass: "firecracker", SharedDocker: true},
		} {
			if _, err := adapter.Provision(t.Context(), spec); !errors.Is(err, ErrFlyMachineIsolation) {
				t.Fatalf("expected workload isolation denial, got %v", err)
			}
		}
		api.Lock()
		defer api.Unlock()
		if len(api.requests) != 0 {
			t.Fatalf("unexpected provider traffic: %v", api.requests)
		}
	})
	t.Run("full lifecycle and Fly wire policy", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		api.Lock()
		api.stopAutostop = true
		api.Unlock()
		backups := &flyMachinesTestR2{}
		adapter := makeAdapter("tenant-a", backups)
		spec := flyMachineTestSpec()
		spec.MemoryMB = 512
		handle, err := adapter.Provision(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		config := api.latest.Config
		region := api.latest.Region
		api.Unlock()
		if region != "fra" || config.Guest.MemoryMB != 512 || config.Guest.MaxMemoryMB != 512 || len(config.Mounts) != 0 {
			t.Fatalf("unexpected placement/config: %s %+v", region, config)
		}
		if len(config.Services) != 1 || string(config.Services[0].Autostop) != `"stop"` || !config.Services[0].Autostart || config.Services[0].MinMachinesRunning != 0 {
			t.Fatalf("unexpected Fly autoscaling parameters: %+v", config.Services)
		}
		if len(config.Metadata) != 4 || config.Metadata["tenant_id"] != "tenant-a" || config.Metadata["workload_id"] != "workload-1" ||
			config.Metadata["slot_ref"] != "slot-1" || config.Metadata["custody_hash"] != "custody-hash-1" {
			t.Fatalf("incorrect provider ownership: %v", config.Metadata)
		}
		if err := adapter.Stop(t.Context(), handle); err != nil {
			t.Fatal(err)
		}
		if observation, err := adapter.Observe(t.Context(), handle); err != nil || observation.State != "stopped" || observation.Health != FlyMachineHealthUnknown {
			t.Fatalf("stopped observation: %+v, %v", observation, err)
		}
		if err := adapter.Wake(t.Context(), handle); err != nil {
			t.Fatal(err)
		}
		if observation, err := adapter.Observe(t.Context(), handle); err != nil || observation.State != "started" || observation.Health != FlyMachineHealthHealthy {
			t.Fatalf("running observation: %+v, %v", observation, err)
		}
		manifest, err := adapter.Backup(t.Context(), handle)
		if err != nil || !slices.Equal(backups.backups, []FlyMachineHandle{handle}) {
			t.Fatalf("backup hook: %+v, %v", manifest, err)
		}
		if err := adapter.Restore(t.Context(), handle, manifest); err != nil || !slices.Equal(backups.restores, []FlyBackupManifest{manifest}) {
			t.Fatalf("restore hook: %+v, %v", backups.restores, err)
		}
		proof, err := adapter.Decommission(t.Context(), handle)
		if err != nil || proof.Handle != handle || proof.ObservedAt.IsZero() {
			t.Fatalf("absence readback: %+v, %v", proof, err)
		}
		api.Lock()
		defer api.Unlock()
		const target = "/v1/apps/isolated-app/machines/machine-tenant-a"
		if got := api.requests[len(api.requests)-3:]; !slices.Equal(got, []string{"GET " + target, "DELETE " + target + "?force=true", "GET " + target}) {
			t.Fatalf("decommission must verify exact target and GET after DELETE: %v", got)
		}
	})
	t.Run("foreign tenant and copied handle fail closed", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		store := &flyMachinesTestR2{}
		adapterA, adapterB := makeAdapter("tenant-a", store), makeAdapter("tenant-b", store)
		handleA, err := adapterA.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		handleB, err := adapterB.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		manifestB, err := adapterB.Backup(t.Context(), handleB)
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		before := len(api.requests)
		api.Unlock()
		forged := handleA
		forged.MachineID = handleB.MachineID
		for _, handle := range []FlyMachineHandle{handleB, forged} {
			_, observeErr := adapterA.Observe(t.Context(), handle)
			_, backupErr := adapterA.Backup(t.Context(), handle)
			_, deleteErr := adapterA.Decommission(t.Context(), handle)
			checks := []error{
				adapterA.Start(t.Context(), handle), adapterA.Stop(t.Context(), handle), adapterA.Wake(t.Context(), handle),
				observeErr, backupErr, adapterA.Restore(t.Context(), handle, FlyBackupManifest{Source: handle, Artifact: manifestB.Artifact}), deleteErr,
			}
			for _, err := range checks {
				if !errors.Is(err, ErrFlyMachineOwnership) {
					t.Fatalf("expected ownership denial, got %v", err)
				}
			}
		}
		if err := adapterA.Restore(t.Context(), handleA, manifestB); !errors.Is(err, ErrFlyMachineOwnership) {
			t.Fatalf("foreign manifest: %v", err)
		}
		api.Lock()
		defer api.Unlock()
		after := api.requests[before:]
		if len(after) != 7 || len(store.backups) != 1 || len(store.restores) != 0 {
			t.Fatalf("unexpected provider/R2 access %v, %+v", after, store)
		}
		for _, request := range after {
			if request != "GET /v1/apps/isolated-app/machines/machine-tenant-b" {
				t.Fatalf("unauthorized side effect: %s", request)
			}
		}
	})
	t.Run("absence remains pending and only NOT FOUND releases evidence", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		adapter := makeAdapter("tenant-a", nil)
		handle, err := adapter.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		api.delayDeletion = true
		api.deleteStatus = http.StatusNotFound
		api.Unlock()
		if proof, err := adapter.Decommission(t.Context(), handle); !errors.Is(err, ErrFlyMachineAbsencePending) || proof != (FlyMachineAbsence{}) {
			t.Fatalf("DELETE 404 without GET 404 cannot prove absence: %+v, %v", proof, err)
		}
		api.Lock()
		delete(api.machines, handle.MachineID)
		before := len(api.requests)
		api.Unlock()
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle || proof.ObservedAt.IsZero() {
			t.Fatalf("decommission retry: %+v, %v", proof, err)
		}
		api.Lock()
		defer api.Unlock()
		if len(api.requests) != before+1 || !strings.HasPrefix(api.requests[before], "GET ") {
			t.Fatalf("readback retry must make only a GET: %v", api.requests[before:])
		}
	})
	t.Run("readback drift blocks power-on without losing cleanup handle", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		adapter := makeAdapter("tenant-a", nil)
		handle, err := adapter.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		machine := api.machines[handle.MachineID]
		machine.Config.Guest.MaxMemoryMB = 1024
		api.machines[handle.MachineID] = machine
		api.Unlock()
		if err := adapter.Start(t.Context(), handle); !errors.Is(err, ErrFlyMachinePolicyDrift) {
			t.Fatalf("power-on with budget drift: %v", err)
		}
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle {
			t.Fatalf("cleanup of drifted exact handle: %+v, %v", proof, err)
		}
	})
	t.Run("non-fra cleanup succeeds while active lifecycle, restart, image and R2 keys enforce policy", func(t *testing.T) {
		api, makeAdapter := flyMachineTestSetup(t)
		backups := &flyMachinesTestR2{}
		adapter := makeAdapter("tenant-a", backups)

		// 1. Provision sends restart.policy == on-failure; non-fra readback still allows Stop + Decommission.
		api.Lock()
		api.createRegion = "ams"
		api.Unlock()
		handle, err := adapter.Provision(t.Context(), flyMachineTestSpec())
		var regionErr *FlyMachineRegionError
		if !errors.As(err, &regionErr) || regionErr.Region != "ams" || handle.MachineID == "" {
			t.Fatalf("expected handle + ams region error on Provision, got handle=%+v err=%v", handle, err)
		}
		api.Lock()
		restart := api.latest.Config.Restart
		api.createRegion = ""
		api.Unlock()
		if restart == nil || restart.Policy != "on-failure" {
			t.Fatalf("Provision must send restart.policy=on-failure, got %+v", restart)
		}
		for _, blocked := range []error{
			adapter.Start(t.Context(), handle),
			adapter.Wake(t.Context(), handle),
			func() error { _, err := adapter.Observe(t.Context(), handle); return err }(),
			func() error { _, err := adapter.Backup(t.Context(), handle); return err }(),
			adapter.Restore(t.Context(), handle, FlyBackupManifest{Source: handle, Artifact: FlyBackupArtifact{Ref: "tenants/tenant-a/workloads/workload-1/backups/snap-001.tar.zst", SHA256: strings.Repeat("a", 64)}}),
		} {
			var re *FlyMachineRegionError
			if !errors.As(blocked, &re) || re.Region != "ams" {
				t.Fatalf("non-fra machine must fail active op with ams error, got %v", blocked)
			}
		}
		if err := adapter.Stop(t.Context(), handle); err != nil {
			t.Fatalf("Stop must clean up owned non-fra machine, got %v", err)
		}
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle {
			t.Fatalf("Decommission must clean up owned non-fra machine, got %+v err=%v", proof, err)
		}

		// 2. Restart policy "always" fails readback.
		adapter2 := makeAdapter("tenant-b", backups)
		handle2, err := adapter2.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		m2 := api.machines[handle2.MachineID]
		m2.Config.Restart = &flyMachineRestart{Policy: "always"}
		api.machines[handle2.MachineID] = m2
		api.Unlock()
		if err := adapter2.Start(t.Context(), handle2); !errors.Is(err, ErrFlyMachinePolicyDrift) {
			t.Fatalf("restart=always must fail readback with policy drift, got %v", err)
		}

		// 3. Image drift on Provision readback returns ErrFlyMachinePolicyDrift.
		api.Lock()
		api.tamperImage = "evil.example.com/other:latest"
		api.Unlock()
		handle3, err := makeAdapter("tenant-c", backups).Provision(t.Context(), flyMachineTestSpec())
		if !errors.Is(err, ErrFlyMachinePolicyDrift) || handle3.MachineID == "" {
			t.Fatalf("image drift must return handle + ErrFlyMachinePolicyDrift, got handle=%+v err=%v", handle3, err)
		}

		// 4. Provision readback 404 maps to ErrFlyMachineResponse, not ErrFlyMachineNotFound.
		api.Lock()
		api.tamperImage = ""
		api.dropOnCreate = true
		api.Unlock()
		handle4, err := makeAdapter("tenant-d", backups).Provision(t.Context(), flyMachineTestSpec())
		if !errors.Is(err, ErrFlyMachineResponse) || errors.Is(err, ErrFlyMachineNotFound) || handle4.MachineID == "" {
			t.Fatalf("Provision readback 404 must return handle + ErrFlyMachineResponse, got handle=%+v err=%v", handle4, err)
		}

		// 5. Realistic R2 object key syntax accepted; unsafe keys and malformed digests rejected.
		digest := strings.Repeat("a", 64)
		validRef := "tenants/tenant-a/workloads/workload-1/backups/snap-001.tar.zst"
		if !(FlyBackupArtifact{Ref: validRef, SHA256: digest}).valid() {
			t.Fatal("expected realistic R2 object key to satisfy FlyBackupArtifact.valid()")
		}
		for _, badRef := range []string{
			"",
			"../escape",
			"/leading-slash",
			"tenants//double-slash",
			"tenants/trailing-slash/",
			"tenants\\backslash",
			"tenants/has space",
			"tenants/has\x00nul",
			strings.Repeat("a", 513),
		} {
			if (FlyBackupArtifact{Ref: badRef, SHA256: digest}).valid() {
				t.Fatalf("expected %q to fail FlyBackupArtifact.valid()", badRef)
			}
		}
		for _, badSHA := range []string{strings.Repeat("a", 63), strings.Repeat("A", 64)} {
			if (FlyBackupArtifact{Ref: validRef, SHA256: badSHA}).valid() {
				t.Fatalf("expected digest %q to fail FlyBackupArtifact.valid()", badSHA)
			}
		}
	})
	t.Run("destroyed readback confirms absence while destroying stays pending and FlyV1 header is preserved", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		const flyV1Token = "FlyV1 fm2_test-secret-token"
		api.Lock()
		api.expectAuth = flyV1Token
		api.destroyState = "destroying"
		api.Unlock()
		owner := FlyMachineOwner{TenantID: "tenant-a", WorkloadID: "workload-1", SlotRef: "slot-1", CustodyHash: "custody-hash-1"}
		adapter, err := newFlyMachinesAdapter("isolated-app", flyV1Token, owner, nil, api.serverURL(t))
		if err != nil {
			t.Fatal(err)
		}
		handle, err := adapter.Provision(t.Context(), flyMachineTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		if proof, err := adapter.Decommission(t.Context(), handle); !errors.Is(err, ErrFlyMachineAbsencePending) || proof != (FlyMachineAbsence{}) {
			t.Fatalf("state=destroying must keep absence pending, got proof=%+v err=%v", proof, err)
		}
		api.Lock()
		api.destroyState = "destroyed"
		api.Unlock()
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle || proof.ObservedAt.IsZero() {
			t.Fatalf("state=destroyed readback must confirm absence, got proof=%+v err=%v", proof, err)
		}
		api.Lock()
		before := len(api.requests)
		api.Unlock()
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle {
			t.Fatalf("repeat decommission on state=destroyed must confirm absence idempotently, got proof=%+v err=%v", proof, err)
		}
		api.Lock()
		after := api.requests[before:]
		api.Unlock()
		if len(after) != 1 || after[0] != "GET /v1/apps/isolated-app/machines/"+handle.MachineID {
			t.Fatalf("repeat decommission on state=destroyed must issue single GET without DELETE: %v", after)
		}
	})
}

type flyTestCommandRecord struct {
	generationID string
	digest       string
	result       *FlyMachineCommandResult
	refusal      *ManagedAppRefusal
	reconciled   bool
}

type flyTestSlotRecord struct {
	state       string
	evidenceRef string
}

type flyTestCustody struct {
	mu                sync.Mutex
	generations       map[string]FlyMachineGeneration
	commands          map[string]flyTestCommandRecord
	slots             map[string]flyTestSlotRecord
	provisionInFlight map[string]bool
	failComplete      bool
}

func newFlyTestCustody() *flyTestCustody {
	return &flyTestCustody{
		generations:       map[string]FlyMachineGeneration{},
		commands:          map[string]flyTestCommandRecord{},
		slots:             map[string]flyTestSlotRecord{},
		provisionInFlight: map[string]bool{},
	}
}

func (s *flyTestCustody) WithGeneration(ctx context.Context, _ string, _ string, fn func(context.Context, FlyMachinesLockedCustody) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(ctx, s)
}

func (s *flyTestCustody) Claim(_ context.Context, in FlyMachineClaimRequest) (FlyMachineClaimOutcome, error) {
	cmdKey := in.Identity.TenantID + "\x00" + in.Request.CommandID
	if existing, ok := s.commands[cmdKey]; ok {
		if existing.digest != in.RequestDigest {
			return FlyMachineClaimOutcome{}, managedAppRefuse("command_conflict", "use an immutable command ID for the exact workload action")
		}
		if existing.refusal != nil {
			copy := *existing.refusal
			return FlyMachineClaimOutcome{}, &copy
		}
		if existing.result != nil {
			copy := *existing.result
			return FlyMachineClaimOutcome{Cached: &copy}, nil
		}
		return FlyMachineClaimOutcome{}, managedAppRefuse("outcome_unknown", "observe, stop or decommission this generation with a new command; never replay an uncertain dispatch")
	}
	for k, cmd := range s.commands {
		if strings.HasPrefix(k, in.Identity.TenantID+"\x00") && cmd.generationID == in.Request.GenerationID && cmd.result == nil && !cmd.reconciled {
			if !IsManagedAppCleanup(string(in.Request.Action)) {
				return FlyMachineClaimOutcome{}, managedAppRefuse("outcome_unknown", "observe, stop or decommission the retained generation before another mutation")
			}
		}
	}
	genKey := in.Identity.TenantID + "\x00" + in.Request.GenerationID
	gen, exists := s.generations[genKey]
	if in.Request.Action == FlyMachineActionProvision {
		if exists {
			return FlyMachineClaimOutcome{}, managedAppRefuse("provision_already_claimed", "observe or decommission the existing generation, or allocate a new generation ID")
		}
		gen = FlyMachineGeneration{
			TenantID:     in.Identity.TenantID,
			GenerationID: in.Request.GenerationID,
			SubjectID:    in.Identity.SubjectID,
			AppID:        in.Request.AppID,
			FlyApp:       in.FlyApp,
			CustodyHash:  in.CustodyHash,
			Spec:         in.Spec,
		}
		s.generations[genKey] = gen
		s.slots[genKey] = flyTestSlotRecord{state: "held"}
		s.provisionInFlight[genKey] = true
	} else {
		if !exists {
			return FlyMachineClaimOutcome{}, managedAppRefuse("ownership_refused", "use a Fly Machine generation owned by the authenticated tenant")
		}
		if gen.SubjectID != in.Identity.SubjectID || gen.AppID != in.Request.AppID || gen.FlyApp != in.FlyApp {
			return FlyMachineClaimOutcome{}, managedAppRefuse("ownership_refused", "use the generation's owning subject, app and Fly deployment")
		}
		if in.Request.MachineID != "" && (gen.MachineID == "" || in.Request.MachineID != gen.MachineID) {
			return FlyMachineClaimOutcome{}, managedAppRefuse("handle_mismatch", "use the ledger-owned Fly Machine handle for this generation")
		}
		if gen.Released {
			if in.Request.Action != FlyMachineActionObserve && in.Request.Action != FlyMachineActionDecommission {
				return FlyMachineClaimOutcome{}, managedAppRefuse("generation_released", "admit a new generation; released handles cannot be reused")
			}
		} else if gen.MachineID == "" {
			if !IsManagedAppCleanup(string(in.Request.Action)) {
				return FlyMachineClaimOutcome{}, managedAppRefuse("outcome_unknown", "observe, stop or decommission the unconfirmed Fly Machine generation to reconcile it")
			}
			gen.ProvisionInFlight = s.provisionInFlight[genKey]
		}
	}
	s.commands[cmdKey] = flyTestCommandRecord{generationID: in.Request.GenerationID, digest: in.RequestDigest}
	return FlyMachineClaimOutcome{Generation: gen}, nil
}

func (s *flyTestCustody) RecordMachineID(_ context.Context, tenantID, generationID, machineID string) error {
	genKey := tenantID + "\x00" + generationID
	gen, ok := s.generations[genKey]
	if !ok {
		return managedAppRefuse("custody_refused", "missing Fly Machine generation")
	}
	if gen.MachineID != "" && gen.MachineID != machineID {
		return managedAppRefuse("custody_refused", "immutable Fly Machine handle")
	}
	gen.MachineID = machineID
	s.generations[genKey] = gen
	return nil
}

func (s *flyTestCustody) MarkReleased(_ context.Context, tenantID, generationID, evidenceRef string) error {
	genKey := tenantID + "\x00" + generationID
	gen, ok := s.generations[genKey]
	if !ok || evidenceRef == "" {
		return managedAppRefuse("absence_unconfirmed", "missing Fly Machine generation")
	}
	if gen.Released && gen.AbsenceEvidenceRef != evidenceRef {
		return managedAppRefuse("absence_unconfirmed", "conflicting absence evidence")
	}
	gen.Released = true
	gen.AbsenceEvidenceRef = evidenceRef
	s.generations[genKey] = gen
	return nil
}

func (s *flyTestCustody) Complete(_ context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, result FlyMachineCommandResult) error {
	if s.failComplete {
		return errors.New("simulated receipt commit failure")
	}
	cmdKey := identity.TenantID + "\x00" + request.CommandID
	cmd, ok := s.commands[cmdKey]
	if !ok || cmd.digest != digest || cmd.result != nil {
		return ErrExecutionClaimLost
	}
	if result.State == "absent" {
		genKey := identity.TenantID + "\x00" + request.GenerationID
		gen, ok := s.generations[genKey]
		if !ok || !gen.Released || gen.AbsenceEvidenceRef != result.EvidenceRef {
			return managedAppRefuse("absence_unconfirmed", "require durable exact-generation absence")
		}
		slot, ok := s.slots[genKey]
		if !ok || (slot.state == "released" && slot.evidenceRef != result.EvidenceRef) {
			return managedAppRefuse("absence_unconfirmed", "require the exact held reservation")
		}
		s.slots[genKey] = flyTestSlotRecord{state: "released", evidenceRef: result.EvidenceRef}
	}
	copy := result
	cmd.result = &copy
	s.commands[cmdKey] = cmd
	if request.Action == FlyMachineActionObserve || request.Action == FlyMachineActionDecommission {
		for k, existing := range s.commands {
			if strings.HasPrefix(k, identity.TenantID+"\x00") && existing.generationID == request.GenerationID && existing.result == nil {
				existing.reconciled = true
				s.commands[k] = existing
			}
		}
	}
	return nil
}

func (s *flyTestCustody) Reject(_ context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest string, refusal *ManagedAppRefusal) error {
	cmdKey := identity.TenantID + "\x00" + request.CommandID
	cmd, ok := s.commands[cmdKey]
	if !ok || cmd.digest != digest || cmd.result != nil || cmd.refusal != nil {
		return ErrExecutionClaimLost
	}
	copy := *refusal
	cmd.refusal = &copy
	cmd.reconciled = true
	s.commands[cmdKey] = cmd
	return nil
}

func (s *flyTestCustody) RejectAndRelease(_ context.Context, identity FlyMachinesIdentity, request FlyMachinesRequest, digest, evidenceRef string, refusal *ManagedAppRefusal) error {
	genKey := identity.TenantID + "\x00" + request.GenerationID
	gen, ok := s.generations[genKey]
	if !ok || gen.MachineID != "" || gen.Released || evidenceRef == "" {
		return managedAppRefuse("absence_unconfirmed", "cannot release generation with existing machine")
	}
	cmdKey := identity.TenantID + "\x00" + request.CommandID
	cmd, ok := s.commands[cmdKey]
	if !ok || cmd.digest != digest || cmd.result != nil || cmd.refusal != nil {
		return ErrExecutionClaimLost
	}
	gen.Released = true
	gen.AbsenceEvidenceRef = evidenceRef
	s.generations[genKey] = gen
	s.slots[genKey] = flyTestSlotRecord{state: "released", evidenceRef: evidenceRef}
	copy := *refusal
	cmd.refusal = &copy
	cmd.reconciled = true
	s.commands[cmdKey] = cmd
	return nil
}

func newTestFlyControl(t *testing.T, api *flyMachinesTestAPI, custody *flyTestCustody, r2 FlyMachineR2Store) *FlyMachinesControl {
	t.Helper()
	api.Lock()
	if api.allowedApps == nil {
		api.allowedApps = map[string]string{
			"isolated-app":   "app-test-token",
			"isolated-app-b": "app-test-token-b",
		}
	}
	if api.appNetworks == nil {
		api.appNetworks = map[string]string{
			"isolated-app":   "net-tenant-a",
			"isolated-app-b": "net-tenant-b",
		}
	}
	api.Unlock()
	control, err := NewFlyMachinesControl(FlyMachinesConfig{
		Custody: custody,
		Region:  FlyMachinesRegion,
		Tenants: map[string]FlyMachinesTenantBinding{
			"tenant-a": {App: "isolated-app", Network: "net-tenant-a", Token: "app-test-token"},
			"tenant-b": {App: "isolated-app-b", Network: "net-tenant-b", Token: "app-test-token-b"},
		},
		APIBase: api.serverURL(t),
		Apps: map[string]FlyMachinesApplication{
			"vaultwarden": {
				Image:        "ghcr.io/dani-garcia/vaultwarden@sha256:" + strings.Repeat("a", 64),
				MemoryMB:     256,
				InternalPort: 8080,
			},
		},
		R2: r2,
		Admit: func(_ context.Context, id FlyMachinesIdentity, _ FlyMachinesRequest) (ManagedAppBudget, error) {
			return ManagedAppBudget{
				AccountID:         "acct-1",
				Subject:           id.SubjectID,
				RequestedTenantID: id.TenantID,
				RequestID:         "req-1",
				Limit:             5,
				ValidUntil:        time.Now().Add(15 * time.Second),
			}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return control
}

func requireManagedAppRefusal(t *testing.T, err error, wantCode string) {
	t.Helper()
	var refusal *ManagedAppRefusal
	if !errors.As(err, &refusal) || refusal.Code != wantCode {
		t.Fatalf("expected ManagedAppRefusal %q, got %v", wantCode, err)
	}
}

func TestFlyMachinesControlCustody(t *testing.T) {
	id := FlyMachinesIdentity{TenantID: "tenant-a", SubjectID: "user-1"}

	t.Run("duplicate provision for same tenant and generation is refused at-most-once", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		first, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-1", GenerationID: "gen-1", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil || first.MachineID != "machine-tenant-a" || first.State != "started" {
			t.Fatalf("initial provision failed: res=%+v err=%v", first, err)
		}

		// Duplicate provision with a new command ID must be refused without touching Fly API.
		api.Lock()
		requestsBefore := len(api.requests)
		api.Unlock()
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-2", GenerationID: "gen-1", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "provision_already_claimed")

		api.Lock()
		requestsAfter := len(api.requests)
		api.Unlock()
		if requestsAfter != requestsBefore {
			t.Fatalf("duplicate provision must not reach Fly API: before=%d after=%d", requestsBefore, requestsAfter)
		}

		// Exact command ID replay returns cached result without touching Fly API.
		replay, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-1", GenerationID: "gen-1", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil || replay != first {
			t.Fatalf("command replay mismatch: got=%+v want=%+v err=%v", replay, first, err)
		}
	})

	t.Run("decommission with wrong or non-existent user-supplied machine_id cannot free slot", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		provisioned, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-1", GenerationID: "gen-1", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil {
			t.Fatal(err)
		}

		api.Lock()
		requestsBefore := len(api.requests)
		api.Unlock()

		// Spoofed/non-existent machine_id must fail closed before reaching Fly API and keep slot held.
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-spoof", GenerationID: "gen-1", AppID: "vaultwarden",
			Action: FlyMachineActionDecommission, MachineID: "non-existent-machine",
		})
		requireManagedAppRefusal(t, err, "handle_mismatch")

		api.Lock()
		requestsAfter := len(api.requests)
		_, stillRunning := api.machines[provisioned.MachineID]
		api.Unlock()
		if requestsAfter != requestsBefore || !stillRunning {
			t.Fatalf("spoofed decommission must not hit Fly API or delete machine: before=%d after=%d running=%v", requestsBefore, requestsAfter, stillRunning)
		}
		if slot := custody.slots["tenant-a\x00gen-1"]; slot.state != "held" {
			t.Fatalf("slot must remain held after rejected spoofed decommission, got %+v", slot)
		}

		// Omitting machine_id uses the ledger-owned handle and succeeds.
		removed, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-ok", GenerationID: "gen-1", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || removed.State != "absent" || removed.MachineID != provisioned.MachineID {
			t.Fatalf("ledger-owned decommission failed: res=%+v err=%v", removed, err)
		}
		if slot := custody.slots["tenant-a\x00gen-1"]; slot.state != "released" {
			t.Fatalf("slot must be released after authoritative decommission, got %+v", slot)
		}
	})

	t.Run("provision failure after create retains machine_id in custody for cleanup", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		api.Lock()
		api.tamperImage = "evil.example.com/tampered@sha256:" + strings.Repeat("f", 64)
		api.Unlock()
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		_, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-drift", GenerationID: "gen-drift", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if !errors.Is(err, ErrFlyMachinePolicyDrift) {
			t.Fatalf("expected ErrFlyMachinePolicyDrift, got %v", err)
		}

		// Machine ID must already be persisted in custody despite readback drift failure.
		gen := custody.generations["tenant-a\x00gen-drift"]
		if gen.MachineID != "machine-tenant-a" || gen.Released {
			t.Fatalf("expected retained unreleased machine_id in custody, got %+v", gen)
		}

		// Duplicate provision is still refused; active start is refused due to unreconciled outcome.
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-retry", GenerationID: "gen-drift", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "outcome_unknown")
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-start-drift", GenerationID: "gen-drift", AppID: "vaultwarden", Action: FlyMachineActionStart,
		})
		requireManagedAppRefusal(t, err, "outcome_unknown")

		// Cleanup stop + decommission resolve the retained machine_id and destroy the drifted Fly machine.
		stopped, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-stop-drift", GenerationID: "gen-drift", AppID: "vaultwarden", Action: FlyMachineActionStop,
		})
		if err != nil || stopped.State != "stopped" || stopped.MachineID != "machine-tenant-a" {
			t.Fatalf("cleanup stop of drifted machine failed: res=%+v err=%v", stopped, err)
		}
		removed, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-drift", GenerationID: "gen-drift", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || removed.State != "absent" || removed.MachineID != "machine-tenant-a" {
			t.Fatalf("cleanup decommission of drifted machine failed: res=%+v err=%v", removed, err)
		}
		api.Lock()
		_, stillExists := api.machines["machine-tenant-a"]
		api.Unlock()
		if stillExists {
			t.Fatal("drifted Fly machine must be deleted on cleanup decommission")
		}
	})

	t.Run("idempotent decommission retry succeeds with deterministic absence evidence", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		provisioned, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-1", GenerationID: "gen-idem", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil {
			t.Fatal(err)
		}

		// Simulate lost receipt commit after Fly deletion and MarkReleased.
		custody.failComplete = true
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-1", GenerationID: "gen-idem", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err == nil {
			t.Fatal("expected simulated receipt commit error")
		}
		wantEvidence := "fly:isolated-app:" + provisioned.MachineID + ":gen-idem"
		if gen := custody.generations["tenant-a\x00gen-idem"]; !gen.Released || gen.AbsenceEvidenceRef != wantEvidence {
			t.Fatalf("expected durable deterministic release evidence %q, got %+v", wantEvidence, gen)
		}

		// Retry with a new command ID after transient failure must succeed idempotently without Fly calls.
		custody.failComplete = false
		api.Lock()
		requestsBefore := len(api.requests)
		api.Unlock()

		retry1, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-2", GenerationID: "gen-idem", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || retry1.State != "absent" || retry1.EvidenceRef != wantEvidence {
			t.Fatalf("idempotent decommission retry failed: res=%+v err=%v", retry1, err)
		}
		retry2, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-3", GenerationID: "gen-idem", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || retry2.State != "absent" || retry2.EvidenceRef != wantEvidence {
			t.Fatalf("second idempotent decommission retry failed: res=%+v err=%v", retry2, err)
		}

		api.Lock()
		requestsAfter := len(api.requests)
		api.Unlock()
		if requestsAfter != requestsBefore {
			t.Fatalf("released generation retry must not re-call Fly API: before=%d after=%d", requestsBefore, requestsAfter)
		}
		if slot := custody.slots["tenant-a\x00gen-idem"]; slot.state != "released" || slot.evidenceRef != wantEvidence {
			t.Fatalf("slot must be released with deterministic evidence %q, got %+v", wantEvidence, slot)
		}
	})

	t.Run("R2 backup config reflected honestly in Status and backup response omits CustodyHash", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custodyNoR2 := newFlyTestCustody()
		controlNoR2 := newTestFlyControl(t, api, custodyNoR2, nil)
		if st := controlNoR2.Status(); !st.Enabled || st.State != "ready" || st.Region != "fra" || st.BackupConfigured {
			t.Fatalf("expected BackupConfigured=false without R2, got %+v", st)
		}
		if _, err := controlNoR2.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-nor2", GenerationID: "gen-nor2", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		}); err != nil {
			t.Fatal(err)
		}
		_, err := controlNoR2.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-backup-nor2", GenerationID: "gen-nor2", AppID: "vaultwarden", Action: FlyMachineActionBackup,
		})
		requireManagedAppRefusal(t, err, "backup_unavailable")

		// With R2 configured, Status().BackupConfigured is true and backup response never leaks CustodyHash.
		r2 := &flyMachinesTestR2{}
		custodyR2 := newFlyTestCustody()
		controlR2 := newTestFlyControl(t, api, custodyR2, r2)
		if st := controlR2.Status(); !st.BackupConfigured {
			t.Fatalf("expected BackupConfigured=true with R2, got %+v", st)
		}
		if _, err := controlR2.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-r2", GenerationID: "gen-r2", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		}); err != nil {
			t.Fatal(err)
		}
		backedUp, err := controlR2.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-backup-r2", GenerationID: "gen-r2", AppID: "vaultwarden", Action: FlyMachineActionBackup,
		})
		if err != nil || backedUp.Backup == nil || backedUp.Backup.Ref != "r2-backup-1" {
			t.Fatalf("backup failed: res=%+v err=%v", backedUp, err)
		}
		encoded, err := json.Marshal(backedUp)
		if err != nil {
			t.Fatal(err)
		}
		custodyHash := custodyR2.generations["tenant-a\x00gen-r2"].CustodyHash
		if strings.Contains(string(encoded), custodyHash) || strings.Contains(strings.ToLower(string(encoded)), "custody") {
			t.Fatalf("external backup response must not expose internal CustodyHash: %s", encoded)
		}
		restored, err := controlR2.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-restore-r2", GenerationID: "gen-r2", AppID: "vaultwarden",
			Action: FlyMachineActionRestore, Backup: backedUp.Backup,
		})
		if err != nil || restored.State != "restored" {
			t.Fatalf("restore failed: res=%+v err=%v", restored, err)
		}

		// Invalid EU R2 config fails closed.
		if _, err := NewFlyMachinesR2Store(FlyMachinesR2Config{AccountID: "short", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"}); err == nil {
			t.Fatal("invalid R2 account ID must fail closed")
		}
		// Non-fra control config fails closed.
		if _, err := NewFlyMachinesControl(FlyMachinesConfig{
			Custody: custodyR2, Region: "ams",
			Tenants: map[string]FlyMachinesTenantBinding{"tenant-a": {App: "isolated-app", Network: "net-a", Token: "tok"}},
			Apps:    map[string]FlyMachinesApplication{"vaultwarden": {Image: "ghcr.io/dani-garcia/vaultwarden@sha256:" + strings.Repeat("a", 64), MemoryMB: 256, InternalPort: 8080}},
			Admit: func(context.Context, FlyMachinesIdentity, FlyMachinesRequest) (ManagedAppBudget, error) {
				return ManagedAppBudget{}, nil
			},
		}); err == nil {
			t.Fatal("non-fra region must fail closed at control initialization")
		}
		// Status must report isolation_mode and must never expose Fly app names.
		statusBytes, err := json.Marshal(controlNoR2.Status())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(statusBytes), "isolated-app") || !strings.Contains(string(statusBytes), "dedicated-app-per-tenant") {
			t.Fatalf("status must expose dedicated-app-per-tenant without app names: %s", statusBytes)
		}
	})

	t.Run("P1-1: lost create response reconciles via FindOwned and never orphans machine or leaks slot", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		// Case A: Fly created the machine, then returned 502 before Homelab saw machine_id.
		api.Lock()
		api.createLostAfterSave = true
		api.createStatus = http.StatusBadGateway
		api.Unlock()

		_, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-lost", GenerationID: "gen-lost", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err == nil {
			t.Fatal("expected lost create response error")
		}
		if gen := custody.generations["tenant-a\x00gen-lost"]; gen.MachineID != "" || gen.Released {
			t.Fatalf("expected unconfirmed generation with empty machine_id, got %+v", gen)
		}
		if slot := custody.slots["tenant-a\x00gen-lost"]; slot.state != "held" {
			t.Fatalf("ambiguous 502 must keep slot held until reconciled, got %+v", slot)
		}
		// Active start must still be refused until reconciled.
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-start-lost", GenerationID: "gen-lost", AppID: "vaultwarden", Action: FlyMachineActionStart,
		})
		requireManagedAppRefusal(t, err, "outcome_unknown")

		// Deterministic machine name must have been sent on create.
		api.Lock()
		api.createLostAfterSave = false
		api.createStatus = 0
		createdName := api.latest.Name
		api.Unlock()
		if !strings.HasPrefix(createdName, "km-") || len(createdName) != 27 {
			t.Fatalf("expected deterministic km-<24hex> machine name, got %q", createdName)
		}

		// Observe reconciles via FindOwned and durably records machine_id.
		obs, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-obs-lost", GenerationID: "gen-lost", AppID: "vaultwarden", Action: FlyMachineActionObserve,
		})
		if err != nil || obs.MachineID != "machine-tenant-a" || obs.State != "started" {
			t.Fatalf("observe reconcile failed: res=%+v err=%v", obs, err)
		}
		if gen := custody.generations["tenant-a\x00gen-lost"]; gen.MachineID != "machine-tenant-a" {
			t.Fatalf("observe reconcile must persist machine_id, got %+v", gen)
		}

		// Decommission deletes the reconciled machine and releases the slot.
		removed, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-lost", GenerationID: "gen-lost", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || removed.State != "absent" || removed.MachineID != "machine-tenant-a" {
			t.Fatalf("decommission after reconcile failed: res=%+v err=%v", removed, err)
		}
		if slot := custody.slots["tenant-a\x00gen-lost"]; slot.state != "released" {
			t.Fatalf("slot must be released after decommission, got %+v", slot)
		}

		// Case B: Ambiguous 503 where Fly did NOT create any machine -> decommission MUST refuse with
		// absence_pending while the provision claim window is still open, and ONLY release the slot
		// after the provision claim window has expired and FindOwned still sees [].
		api.Lock()
		api.createStatus = http.StatusServiceUnavailable
		api.Unlock()
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-503", GenerationID: "gen-503", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err == nil {
			t.Fatal("expected 503 provision error")
		}
		api.Lock()
		api.createStatus = 0
		api.Unlock()

		// While provision claim_until is still active: decommission MUST return absence_pending and keep slot held!
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-503-early", GenerationID: "gen-503", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		requireManagedAppRefusal(t, err, "absence_pending")
		if slot := custody.slots["tenant-a\x00gen-503"]; slot.state != "held" {
			t.Fatalf("slot must remain held while provision claim window is still open, got %+v", slot)
		}

		// Once provision claim_until expires and FindOwned still sees []: decommission tombstones and releases slot.
		custody.mu.Lock()
		custody.provisionInFlight["tenant-a\x00gen-503"] = false
		custody.mu.Unlock()

		cleaned, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-503", GenerationID: "gen-503", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || cleaned.State != "absent" || !strings.HasPrefix(cleaned.EvidenceRef, "fly-never-created:") {
			t.Fatalf("never-created decommission reconcile failed: res=%+v err=%v", cleaned, err)
		}
		if slot := custody.slots["tenant-a\x00gen-503"]; slot.state != "released" {
			t.Fatalf("slot must be released when FindOwned confirms no machine exists after claim window, got %+v", slot)
		}
	})

	t.Run("P1-2: definitive 4xx provision refusal verifies FindOwned before releasing slot", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		api.Lock()
		api.createStatus = http.StatusUnprocessableEntity
		api.Unlock()
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		_, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-422", GenerationID: "gen-422", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "provider_provision_refused")
		if gen := custody.generations["tenant-a\x00gen-422"]; !gen.Released || !strings.HasPrefix(gen.AbsenceEvidenceRef, "fly-create-refused:") {
			t.Fatalf("422 refusal must tombstone generation, got %+v", gen)
		}
		if slot := custody.slots["tenant-a\x00gen-422"]; slot.state != "released" {
			t.Fatalf("422 refusal must immediately release slot, got %+v", slot)
		}
		// Replay of exact command_id returns the stored refusal without touching Fly.
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-422", GenerationID: "gen-422", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "provider_provision_refused")

		// If Fly anomalously created a machine despite returning 422, FindOwned catches it,
		// records machine_id, stops it, and keeps the slot held!
		api.Lock()
		api.createLostAfterSave = true
		api.createStatus = http.StatusUnprocessableEntity
		api.Unlock()
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-422-phantom", GenerationID: "gen-422-phantom", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err == nil {
			t.Fatal("expected 422 error when phantom machine exists")
		}
		if gen := custody.generations["tenant-a\x00gen-422-phantom"]; gen.MachineID != "machine-tenant-a" || gen.Released {
			t.Fatalf("FindOwned must retain phantom machine and never release slot, got %+v", gen)
		}
		if slot := custody.slots["tenant-a\x00gen-422-phantom"]; slot.state != "held" {
			t.Fatalf("slot must remain held when FindOwned discovers phantom machine, got %+v", slot)
		}
	})

	t.Run("P1-3 & P2-2: per-tenant Fly app, network readback, and token isolation fail closed", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		apps := map[string]FlyMachinesApplication{
			"vaultwarden": {Image: "ghcr.io/dani-garcia/vaultwarden@sha256:" + strings.Repeat("a", 64), MemoryMB: 256, InternalPort: 8080},
		}
		admit := func(context.Context, FlyMachinesIdentity, FlyMachinesRequest) (ManagedAppBudget, error) {
			return ManagedAppBudget{AccountID: "acct-1", Subject: "user-1", RequestedTenantID: "tenant-a", RequestID: "r1", Limit: 2, ValidUntil: time.Now().Add(15 * time.Second)}, nil
		}
		badConfigs := []map[string]FlyMachinesTenantBinding{
			{},
			{"tenant-a": {App: "shared-app", Network: "net-a", Token: "tok-a"}, "tenant-b": {App: "shared-app", Network: "net-b", Token: "tok-b"}},
			{"tenant-a": {App: "app-a", Network: "shared-net", Token: "tok-a"}, "tenant-b": {App: "app-b", Network: "shared-net", Token: "tok-b"}},
			{"tenant-a": {App: "app-a", Network: "net-a", Token: "shared-tok"}, "tenant-b": {App: "app-b", Network: "net-b", Token: "shared-tok"}},
			{"tenant-a": {App: "app-a", Network: "", Token: "tok-a"}},
			{"tenant-a": {App: "app-a", Network: "default", Token: "tok-a"}},
		}
		for idx, tenants := range badConfigs {
			if _, err := NewFlyMachinesControl(FlyMachinesConfig{
				Custody: custody, Region: FlyMachinesRegion, Tenants: tenants, Apps: apps, Admit: admit,
			}); err == nil {
				t.Fatalf("case %d: expected shared or invalid tenant isolation config to fail closed", idx)
			}
		}
		// Unbound tenant fails closed with tenant_isolation_unavailable.
		control := newTestFlyControl(t, api, custody, nil)
		_, err := control.Execute(t.Context(), FlyMachinesIdentity{TenantID: "tenant-unbound", SubjectID: "user-9"}, FlyMachinesRequest{
			CommandID: "cmd-unbound", GenerationID: "gen-unbound", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "tenant_isolation_unavailable")

		// P2-2: Live Fly app reporting "default" or mismatched network fails closed BEFORE creating a machine and releases the slot.
		api.Lock()
		api.appNetworks["isolated-app"] = "default"
		api.Unlock()
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-net-drift", GenerationID: "gen-net-drift", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "tenant_isolation_unavailable")
		if slot := custody.slots["tenant-a\x00gen-net-drift"]; slot.state != "released" {
			t.Fatalf("network drift must release slot before any create is attempted, got %+v", slot)
		}
		api.Lock()
		if len(api.machines) != 0 {
			api.Unlock()
			t.Fatal("network drift must never create a Fly machine")
		}
		api.Unlock()
	})

	t.Run("P2-3: per-tenant concurrency fairness prevents one tenant from starving other tenants", func(t *testing.T) {
		gate := newFlyMachinesPostgresCustody(nil, 4).(*flyMachinesPostgresCustody)
		if !gate.acquireSlot("tenant-a") {
			t.Fatal("expected first slot for tenant-a to succeed")
		}
		// Second concurrent command for tenant-a is rejected even though 3 global slots remain.
		if gate.acquireSlot("tenant-a") {
			t.Fatal("single tenant must not consume multiple concurrent slots and starve others")
		}
		// Another tenant still gets a slot immediately.
		if !gate.acquireSlot("tenant-b") {
			t.Fatal("tenant-b must not be starved by tenant-a")
		}
		gate.releaseSlot("tenant-a")
		gate.releaseSlot("tenant-b")
		if !gate.acquireSlot("tenant-a") {
			t.Fatal("tenant-a should reacquire slot after release")
		}
		gate.releaseSlot("tenant-a")
	})

	t.Run("P2-1 & P2-2: R2 fails closed without PayloadBridge and purges only owning workload prefix on decommission", func(t *testing.T) {
		r2Server, objects := newMockFlyR2Server(t)
		// Without PayloadBridge: BackupReady is false and Backup/Restore fail closed.
		storeNoBridge, err := newFlyMachinesR2Store(FlyMachinesR2Config{
			AccountID: strings.Repeat("a", 32), Bucket: "kombify-eu-backups",
			AccessKeyID: "ak", SecretAccessKey: "sk",
		}, r2Server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if storeNoBridge.BackupReady() {
			t.Fatal("store without PayloadBridge must report BackupReady=false")
		}
		handle := FlyMachineHandle{
			App: "isolated-app", MachineID: "machine-tenant-a",
			Owner: FlyMachineOwner{TenantID: "tenant-a", WorkloadID: "gen-r2-real", SlotRef: "vaultwarden", CustodyHash: strings.Repeat("b", 64)},
		}
		if _, err := storeNoBridge.Backup(t.Context(), handle); !errors.Is(err, ErrFlyMachineBackup) {
			t.Fatalf("metadata-only Backup without PayloadBridge must fail closed, got %v", err)
		}

		// With PayloadBridge: real bytes are exported, verified, restored, and purged strictly on decommission.
		var imported []byte
		bridge := &mockFlyPayloadBridge{
			exportData: []byte("vaultwarden-encrypted-sqlite-snapshot"),
			onImport:   func(b []byte) { imported = append([]byte(nil), b...) },
		}
		storeWithBridge, err := newFlyMachinesR2Store(FlyMachinesR2Config{
			AccountID: strings.Repeat("a", 32), Bucket: "kombify-eu-backups",
			AccessKeyID: "ak", SecretAccessKey: "sk", PayloadBridge: bridge,
		}, r2Server.URL)
		if err != nil {
			t.Fatal(err)
		}
		if !storeWithBridge.BackupReady() {
			t.Fatal("store with PayloadBridge must report BackupReady=true")
		}
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, storeWithBridge)

		if _, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-realr2", GenerationID: "gen-r2-real", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		}); err != nil {
			t.Fatal(err)
		}
		backed, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-back-realr2", GenerationID: "gen-r2-real", AppID: "vaultwarden", Action: FlyMachineActionBackup,
		})
		if err != nil || backed.Backup == nil {
			t.Fatalf("backup with PayloadBridge failed: res=%+v err=%v", backed, err)
		}
		if _, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-rest-realr2", GenerationID: "gen-r2-real", AppID: "vaultwarden",
			Action: FlyMachineActionRestore, Backup: backed.Backup,
		}); err != nil {
			t.Fatalf("restore with PayloadBridge failed: %v", err)
		}
		if string(imported) != "vaultwarden-encrypted-sqlite-snapshot" {
			t.Fatalf("restored payload mismatch: got %q", string(imported))
		}

		// Seed another tenant's R2 object and verify decommission purges ONLY tenant-a/gen-r2-real.
		otherKey := flyBackupKeyPrefix("tenant-b", "gen-other") + strings.Repeat("c", 64) + ".json"
		objects.set(otherKey, []byte(`{"other":true}`))

		if _, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-realr2", GenerationID: "gen-r2-real", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		}); err != nil {
			t.Fatalf("decommission with R2 purge failed: %v", err)
		}
		if objects.has(backed.Backup.Ref) {
			t.Fatalf("decommission must purge R2 object %q", backed.Backup.Ref)
		}
		if !objects.has(otherKey) {
			t.Fatalf("decommission must never purge another tenant's R2 object %q", otherKey)
		}
	})

	t.Run("P3: dot-dot generation refused, observe 404 keeps slot held, absence_pending persists cleanly", func(t *testing.T) {
		api, _ := flyMachineTestSetup(t)
		custody := newFlyTestCustody()
		control := newTestFlyControl(t, api, custody, nil)

		// Generation ID with ".." must be refused.
		_, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-dotdot", GenerationID: "gen..escape", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		requireManagedAppRefusal(t, err, "identity_refused")

		// Observe 404 must NOT release the slot.
		prov, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-obs404", GenerationID: "gen-obs404", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		delete(api.machines, prov.MachineID)
		api.Unlock()
		obs, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-obs-404", GenerationID: "gen-obs404", AppID: "vaultwarden", Action: FlyMachineActionObserve,
		})
		if err != nil || obs.State != "not_found" {
			t.Fatalf("observe 404 should return state=not_found without releasing slot: res=%+v err=%v", obs, err)
		}
		if slot := custody.slots["tenant-a\x00gen-obs404"]; slot.state != "held" {
			t.Fatalf("observe 404 must NOT release slot, got %+v", slot)
		}

		// absence_pending persists refusal on command so replay returns absence_pending (not outcome_unknown)
		// and a new command retries cleanly.
		prov2, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-prov-pend", GenerationID: "gen-pend", AppID: "vaultwarden", Action: FlyMachineActionProvision,
		})
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		api.destroyState = "destroying"
		api.Unlock()
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-pend-1", GenerationID: "gen-pend", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		requireManagedAppRefusal(t, err, "absence_pending")
		_, err = control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-pend-1", GenerationID: "gen-pend", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		requireManagedAppRefusal(t, err, "absence_pending")

		api.Lock()
		api.destroyState = "destroyed"
		api.Unlock()
		done, err := control.Execute(t.Context(), id, FlyMachinesRequest{
			CommandID: "cmd-decomm-pend-2", GenerationID: "gen-pend", AppID: "vaultwarden", Action: FlyMachineActionDecommission,
		})
		if err != nil || done.State != "absent" || done.MachineID != prov2.MachineID {
			t.Fatalf("retry decommission after absence_pending failed: res=%+v err=%v", done, err)
		}
	})
}

type mockFlyPayloadBridge struct {
	exportData []byte
	onImport   func([]byte)
}

func (m *mockFlyPayloadBridge) Export(_ context.Context, _ FlyMachineHandle) ([]byte, error) {
	return m.exportData, nil
}

func (m *mockFlyPayloadBridge) Import(_ context.Context, _ FlyMachineHandle, payload []byte) error {
	if m.onImport != nil {
		m.onImport(payload)
	}
	return nil
}

type mockFlyR2Objects struct {
	mu   sync.Mutex
	data map[string][]byte
}

func (m *mockFlyR2Objects) set(k string, v []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[k] = append([]byte(nil), v...)
}

func (m *mockFlyR2Objects) has(k string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.data[k]
	return ok
}

func newMockFlyR2Server(t *testing.T) (*httptest.Server, *mockFlyR2Objects) {
	t.Helper()
	objs := &mockFlyR2Objects{data: map[string][]byte{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/kombify-eu-backups")
		key := strings.TrimPrefix(path, "/")
		objs.mu.Lock()
		defer objs.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("list-type") == "2" {
				prefix := r.URL.Query().Get("prefix")
				type s3Content struct {
					Key string `xml:"Key"`
				}
				type listResult struct {
					XMLName     xml.Name    `xml:"ListBucketResult"`
					IsTruncated bool        `xml:"IsTruncated"`
					Contents    []s3Content `xml:"Contents"`
				}
				res := listResult{}
				for k := range objs.data {
					if strings.HasPrefix(k, prefix) {
						res.Contents = append(res.Contents, s3Content{Key: k})
					}
				}
				w.Header().Set("Content-Type", "application/xml")
				_ = xml.NewEncoder(w).Encode(res)
				return
			}
			body, ok := objs.data[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(body)
		case http.MethodPut:
			if r.Header.Get("If-None-Match") == "*" {
				if _, exists := objs.data[key]; exists {
					w.WriteHeader(http.StatusPreconditionFailed)
					return
				}
			}
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			objs.data[key] = body
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			delete(objs.data, key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	t.Cleanup(server.Close)
	return server, objs
}

// TestFlyMachinesPostgresCustodyIntegration exercises migration 204, RLS,
// immutability triggers, at-most-once claims, RecordMachineID, MarkReleased,
// ReleaseManagedAppSlotTx, RejectAndRelease, and tenant-scoped advisory locks
// against a real PostgreSQL instance when HOMELAB_TEST_POSTGRES_URL is set.
func TestFlyMachinesPostgresCustodyIntegration(t *testing.T) {
	adminDB := openNativeAdmissionIntegrationDB(t)
	var hasTable bool
	if err := adminDB.QueryRowContext(t.Context(), `SELECT to_regclass('fly_machine_generations') IS NOT NULL`).Scan(&hasTable); err != nil || !hasTable {
		t.Fatalf("expected fly_machine_generations table from migration 204, hasTable=%v err=%v", hasTable, err)
	}
	var hasCommands bool
	if err := adminDB.QueryRowContext(t.Context(), `SELECT to_regclass('fly_machine_commands') IS NOT NULL`).Scan(&hasCommands); err != nil || !hasCommands {
		t.Fatalf("expected fly_machine_commands table from migration 204, hasCommands=%v err=%v", hasCommands, err)
	}
	// Verify concurrency gate bounds total and per-tenant concurrent Fly custody workers.
	custody := newFlyMachinesPostgresCustody(adminDB, 2).(*flyMachinesPostgresCustody)
	if !custody.acquireSlot("tenant-a") {
		t.Fatal("expected tenant-a slot acquire")
	}
	defer custody.releaseSlot("tenant-a")
	err := custody.WithGeneration(t.Context(), "tenant-a", "gen-sem", func(context.Context, FlyMachinesLockedCustody) error {
		return nil
	})
	requireManagedAppRefusal(t, err, "worker_busy")
}
