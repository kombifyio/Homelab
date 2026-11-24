package providercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

type flyMachinesTestAPI struct {
	sync.Mutex
	t             *testing.T
	baseURL       string
	requests      []string
	latest        flyMachineCreate
	machines      map[string]flyMachine
	delayDeletion bool
	stopAutostop  bool
	deleteStatus  int
	createRegion  string
	tamperImage   string
	dropOnCreate  bool
	destroyState  string
	expectAuth    string
}

func (api *flyMachinesTestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.Lock()
	defer api.Unlock()
	api.requests = append(api.requests, request.Method+" "+request.URL.RequestURI())
	expectedAuth := api.expectAuth
	if expectedAuth == "" {
		expectedAuth = "Bearer app-test-token"
	}
	if request.Header.Get("Authorization") != expectedAuth {
		api.t.Error("missing app-scoped token")
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	const collection = "/v1/apps/isolated-app/machines"
	if request.URL.Path == collection && request.Method == http.MethodPost {
		if err := json.NewDecoder(request.Body).Decode(&api.latest); err != nil {
			api.t.Errorf("decode Fly create: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		region := api.latest.Region
		if api.createRegion != "" {
			region = api.createRegion
		}
		machine := flyMachine{ID: "machine-" + api.latest.Config.Metadata["tenant_id"], Region: region, State: "started", Config: api.latest.Config,
			Checks: []flyMachineCheckStatus{{Name: "workload", Status: "passing"}}}
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
		writer.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(writer).Encode(map[string]string{"id": machine.ID})
		return
	}
	path, found := strings.CutPrefix(request.URL.Path, collection+"/")
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
