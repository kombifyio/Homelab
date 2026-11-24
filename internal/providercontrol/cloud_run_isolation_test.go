package providercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

const (
	cloudRunTestProject  = "test-project-1"
	cloudRunTestToken    = "cloud-run-test-token"
	cloudRunTestImage    = "europe-west3-docker.pkg.dev/example/workloads/app:1"
	cloudRunTestPrefix   = "/v2/projects/test-project-1/locations/"
	cloudRunTestBase     = "/v2/projects/test-project-1/locations/europe-west3/services"
)

func cloudRunTestKey(location, serviceID string) string {
	return location + "/" + serviceID
}

type cloudRunTestAPI struct {
	sync.Mutex
	t              *testing.T
	requests       []string
	latest         cloudRunService
	services       map[string]cloudRunService
	delayDeletion  bool
	deleteStatus   int
	createStatus   int
	createLocation string
	tamperImage    string
	dropOnCreate   bool
}

func (api *cloudRunTestAPI) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	api.Lock()
	defer api.Unlock()
	api.requests = append(api.requests, request.Method+" "+request.URL.RequestURI())
	if request.Header.Get("Authorization") != "Bearer "+cloudRunTestToken {
		api.t.Error("missing project-scoped token")
		writer.WriteHeader(http.StatusForbidden)
		return
	}
	rest, ok := strings.CutPrefix(request.URL.Path, cloudRunTestPrefix)
	if !ok {
		api.t.Errorf("unexpected Cloud Run path: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	parts := strings.Split(rest, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] == "services" && request.Method == http.MethodPost {
		if api.createStatus != 0 {
			writer.WriteHeader(api.createStatus)
			return
		}
		serviceID := request.URL.Query().Get("serviceId")
		if err := json.NewDecoder(request.Body).Decode(&api.latest); err != nil {
			api.t.Errorf("decode Cloud Run create: %v", err)
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		reqLocation := parts[0]
		respLocation := reqLocation
		if api.createLocation != "" {
			respLocation = api.createLocation
		}
		created := api.latest
		created.Name = "projects/" + cloudRunTestProject + "/locations/" + respLocation + "/services/" + serviceID
		created.TerminalCondition = &cloudRunCondition{Type: "Ready", State: cloudRunConditionSucceeded}
		if api.tamperImage != "" {
			created.Template.Containers = slices.Clone(created.Template.Containers)
			created.Template.Containers[0].Image = api.tamperImage
		}
		if !api.dropOnCreate {
			api.services[cloudRunTestKey(reqLocation, serviceID)] = created
		}
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]string{"name": "projects/" + cloudRunTestProject + "/locations/" + respLocation + "/operations/create-1"})
		return
	}
	if len(parts) != 3 || parts[0] == "" || parts[1] != "services" || parts[2] == "" {
		api.t.Errorf("unexpected Cloud Run path: %s", request.URL.Path)
		writer.WriteHeader(http.StatusNotFound)
		return
	}
	reqLocation, serviceID := parts[0], parts[2]
	key := cloudRunTestKey(reqLocation, serviceID)
	service, exists := api.services[key]
	switch request.Method {
	case http.MethodGet:
		if !exists {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(writer).Encode(service)
	case http.MethodDelete:
		if api.deleteStatus != 0 {
			writer.WriteHeader(api.deleteStatus)
			return
		}
		if !exists {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if !api.delayDeletion {
			delete(api.services, key)
		}
		writer.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(writer).Encode(map[string]string{"name": "operations/delete-1"})
	default:
		api.t.Errorf("unexpected Cloud Run operation: %s %s", request.Method, request.URL.Path)
		writer.WriteHeader(http.StatusBadRequest)
	}
}

type cloudRunTestR2 struct {
	backups  []CloudRunHandle
	restores []CloudRunBackupManifest
}

func (store *cloudRunTestR2) Backup(_ context.Context, target CloudRunHandle) (CloudRunBackupArtifact, error) {
	store.backups = append(store.backups, target)
	return CloudRunBackupArtifact{Ref: "r2-backup-1", SHA256: strings.Repeat("a", 64)}, nil
}

func (store *cloudRunTestR2) Restore(_ context.Context, target CloudRunHandle, artifact CloudRunBackupArtifact) error {
	store.restores = append(store.restores, CloudRunBackupManifest{Source: target, Artifact: artifact})
	return nil
}

type cloudRunBadR2 struct {
	artifact CloudRunBackupArtifact
	err      error
}

func (store cloudRunBadR2) Backup(context.Context, CloudRunHandle) (CloudRunBackupArtifact, error) {
	return store.artifact, store.err
}

func (store cloudRunBadR2) Restore(context.Context, CloudRunHandle, CloudRunBackupArtifact) error {
	return store.err
}

func cloudRunTestOwner(tenant string) CloudRunOwner {
	return CloudRunOwner{TenantID: tenant, WorkloadID: "workload-1", SlotRef: "slot-1", CustodyHash: "custody-hash-1"}
}

func cloudRunTestSetup(t *testing.T) (*cloudRunTestAPI, func(string, CloudRunR2Store) *CloudRunAdapter) {
	t.Helper()
	api := &cloudRunTestAPI{t: t, services: map[string]cloudRunService{}}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)
	return api, func(tenant string, store CloudRunR2Store) *CloudRunAdapter {
		t.Helper()
		adapter, err := newCloudRunAdapter(cloudRunTestProject, "svc-"+tenant, cloudRunTestToken, cloudRunTestOwner(tenant), store, server.URL+"/v2")
		if err != nil {
			t.Fatal(err)
		}
		return adapter
	}
}

func cloudRunTestSpec() CloudRunSpec {
	return CloudRunSpec{Region: "europe-west3", IsolationClass: "microvm", MemoryMB: 512, Image: cloudRunTestImage, InternalPort: 8080}
}

func TestCloudRunBoundary(t *testing.T) {
	t.Run("invalid policy never reaches provider", func(t *testing.T) {
		api, makeAdapter := cloudRunTestSetup(t)
		adapter := makeAdapter("tenant-a", nil)
		for _, region := range []string{"", "europe-west1", "EUROPE-WEST3", "europe-west3 ", "us-central1"} {
			spec := cloudRunTestSpec()
			spec.Region = region
			_, err := adapter.Provision(t.Context(), spec)
			var denied *CloudRunRegionError
			if !errors.As(err, &denied) || denied.Region != region {
				t.Fatalf("region %q: expected explicit placement denial, got %v", region, err)
			}
		}
		for _, memory := range []int{0, 127, 128, 256, 384, 513, 1024, 2048} {
			spec := cloudRunTestSpec()
			spec.MemoryMB = memory
			if _, err := adapter.Provision(t.Context(), spec); !errors.Is(err, ErrCloudRunMemory) {
				t.Fatalf("memory %d: expected Cloud Run size denial, got %v", memory, err)
			}
		}
		for _, mutate := range []func(*CloudRunSpec){
			func(spec *CloudRunSpec) { spec.IsolationClass = "docker" },
			func(spec *CloudRunSpec) { spec.IsolationClass = "gvisor" },
			func(spec *CloudRunSpec) { spec.IsolationClass = "firecracker" },
			func(spec *CloudRunSpec) { spec.IsolationClass = "gen1" },
			func(spec *CloudRunSpec) { spec.IsolationClass = "" },
			func(spec *CloudRunSpec) { spec.DockerSocket = true },
			func(spec *CloudRunSpec) { spec.SharedDocker = true },
		} {
			spec := cloudRunTestSpec()
			mutate(&spec)
			if _, err := adapter.Provision(t.Context(), spec); !errors.Is(err, ErrCloudRunIsolation) {
				t.Fatalf("expected workload isolation denial, got %v", err)
			}
		}
		for _, mutate := range []func(*CloudRunSpec){
			func(spec *CloudRunSpec) { spec.RejectedVPCConnector = "projects/p/locations/l/connectors/c" },
			func(spec *CloudRunSpec) { spec.RejectedCloudSQL = []string{"p:europe-west3:db"} },
			func(spec *CloudRunSpec) { spec.RejectedVolumes = []string{"gcs:bucket"} },
			func(spec *CloudRunSpec) { spec.RejectedBindings = []string{"secretmanager:key"} },
			func(spec *CloudRunSpec) { spec.Image = "" },
			func(spec *CloudRunSpec) { spec.Image = "image with space" },
			func(spec *CloudRunSpec) { spec.Image = "-flag" },
			func(spec *CloudRunSpec) { spec.Image = "example.com/app:1\nevil" },
			func(spec *CloudRunSpec) { spec.InternalPort = 0 },
			func(spec *CloudRunSpec) { spec.InternalPort = 65536 },
			func(spec *CloudRunSpec) { spec.Ingress = "INGRESS_TRAFFIC_ALL" },
			func(spec *CloudRunSpec) { spec.Ingress = "internal" },
		} {
			spec := cloudRunTestSpec()
			mutate(&spec)
			if _, err := adapter.Provision(t.Context(), spec); !errors.Is(err, ErrCloudRunSpec) {
				t.Fatalf("expected specification denial for %+v, got %v", spec, err)
			}
		}
		api.Lock()
		defer api.Unlock()
		if len(api.requests) != 0 {
			t.Fatalf("unexpected provider traffic: %v", api.requests)
		}
	})
	t.Run("constructor refuses unsafe identifiers and owner labels", func(t *testing.T) {
		owner := cloudRunTestOwner("tenant-a")
		for _, project := range []string{"", "short", "Upper-case-project", "1-starts-with-digit", "ends-with-dash-", "has/slash-project", "has.dot-project", strings.Repeat("a", 31)} {
			if _, err := newCloudRunAdapter(project, "svc", cloudRunTestToken, owner, nil, "http://127.0.0.1"); !errors.Is(err, ErrCloudRunSpec) {
				t.Fatalf("project %q: expected specification denial, got %v", project, err)
			}
		}
		for _, name := range []string{"", "Upper", "1svc", "svc-", "svc/other", "svc..x", "svc?x=1", strings.Repeat("a", 50)} {
			if _, err := newCloudRunAdapter(cloudRunTestProject, name, cloudRunTestToken, owner, nil, "http://127.0.0.1"); !errors.Is(err, ErrCloudRunSpec) {
				t.Fatalf("service %q: expected specification denial, got %v", name, err)
			}
		}
		for _, token := range []string{"", "token\nHeader: x", "token with space"} {
			if _, err := newCloudRunAdapter(cloudRunTestProject, "svc", token, owner, nil, "http://127.0.0.1"); !errors.Is(err, ErrCloudRunSpec) {
				t.Fatalf("token %q: expected specification denial, got %v", token, err)
			}
		}
		for _, mutate := range []func(*CloudRunOwner){
			func(o *CloudRunOwner) { o.TenantID = "" },
			func(o *CloudRunOwner) { o.WorkloadID = " workload-1" },
			func(o *CloudRunOwner) { o.SlotRef = "slot-1 " },
			func(o *CloudRunOwner) { o.CustodyHash = "Custody-Hash" },
			func(o *CloudRunOwner) { o.TenantID = "tenant.a" },
			func(o *CloudRunOwner) { o.TenantID = strings.Repeat("a", 64) },
		} {
			bad := owner
			mutate(&bad)
			if _, err := newCloudRunAdapter(cloudRunTestProject, "svc", cloudRunTestToken, bad, nil, "http://127.0.0.1"); !errors.Is(err, ErrCloudRunOwnership) {
				t.Fatalf("owner %+v: expected ownership denial, got %v", bad, err)
			}
		}
	})
	t.Run("full lifecycle and Cloud Run wire policy", func(t *testing.T) {
		api, makeAdapter := cloudRunTestSetup(t)
		backups := &cloudRunTestR2{}
		adapter := makeAdapter("tenant-a", backups)
		spec := cloudRunTestSpec()
		spec.MemoryMB = 512
		handle, err := adapter.Provision(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		const target = cloudRunTestBase + "/svc-tenant-a"
		api.Lock()
		if len(api.requests) != 2 || api.requests[0] != "POST "+cloudRunTestBase+"?serviceId=svc-tenant-a" || api.requests[1] != "GET "+target {
			t.Fatalf("provision must be one create in europe-west3 and a separate readback: %v", api.requests)
		}
		create := api.latest
		api.Unlock()
		template := create.Template
		if create.Ingress != "INGRESS_TRAFFIC_INTERNAL_ONLY" || template.ExecutionEnvironment != "EXECUTION_ENVIRONMENT_GEN2" ||
			template.Scaling.MinInstanceCount != 0 || template.Scaling.MaxInstanceCount != 1 ||
			len(template.Volumes) != 0 || !cloudRunRawAbsent(template.VPCAccess) || len(template.Containers) != 1 {
			t.Fatalf("unexpected placement/config: %+v", create)
		}
		container := template.Containers[0]
		if container.Image != cloudRunTestImage || len(container.Ports) != 1 || container.Ports[0].ContainerPort != 8080 ||
			container.Resources.Limits["cpu"] != "1000m" || container.Resources.Limits["memory"] != "512Mi" ||
			len(container.Resources.Limits) != 2 || !container.Resources.CPUIdle || len(container.VolumeMounts) != 0 {
			t.Fatalf("unexpected container resources: %+v", container)
		}
		if len(create.Labels) != 4 || create.Labels["tenant_id"] != "tenant-a" || create.Labels["workload_id"] != "workload-1" ||
			create.Labels["slot_ref"] != "slot-1" || create.Labels["custody_hash"] != "custody-hash-1" {
			t.Fatalf("incorrect provider ownership: %v", create.Labels)
		}
		if handle != (CloudRunHandle{Project: cloudRunTestProject, Location: CloudRunRegion, ServiceName: "svc-tenant-a", Owner: cloudRunTestOwner("tenant-a")}) {
			t.Fatalf("unexpected handle: %+v", handle)
		}
		lbSpec := cloudRunTestSpec()
		lbSpec.Ingress = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
		lbAdapter := makeAdapter("tenant-lb", backups)
		lbHandle, err := lbAdapter.Provision(t.Context(), lbSpec)
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		lbIngress := api.latest.Ingress
		api.Unlock()
		if lbIngress != "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER" || lbHandle.Location != CloudRunRegion {
			t.Fatalf("internal load balancer ingress not wired: ingress=%q handle=%+v", lbIngress, lbHandle)
		}
		if proof, err := lbAdapter.Decommission(t.Context(), lbHandle); err != nil || proof.Handle != lbHandle {
			t.Fatalf("decommission internal lb test service: %+v, %v", proof, err)
		}
		if err := adapter.Start(t.Context(), handle); err != nil {
			t.Fatal(err)
		}
		if err := adapter.Stop(t.Context(), handle); err != nil {
			t.Fatal(err)
		}
		if err := adapter.Wake(t.Context(), handle); err != nil {
			t.Fatal(err)
		}
		if observation, err := adapter.Observe(t.Context(), handle); err != nil || observation.State != "ready" || observation.Health != CloudRunHealthHealthy {
			t.Fatalf("ready observation: %+v, %v", observation, err)
		}
		for _, condition := range []struct {
			state       *cloudRunCondition
			reconciling bool
			wantState   string
			wantHealth  CloudRunHealth
		}{
			{&cloudRunCondition{Type: "Ready", State: "CONDITION_RECONCILING"}, true, "reconciling", CloudRunHealthUnknown},
			{&cloudRunCondition{Type: "Ready", State: cloudRunConditionFailed}, false, "failed", CloudRunHealthUnhealthy},
			{nil, false, "reconciling", CloudRunHealthUnknown},
		} {
			api.Lock()
			service := api.services[cloudRunTestKey(CloudRunRegion, "svc-tenant-a")]
			service.TerminalCondition = condition.state
			service.Reconciling = condition.reconciling
			api.services[cloudRunTestKey(CloudRunRegion, "svc-tenant-a")] = service
			api.Unlock()
			if observation, err := adapter.Observe(t.Context(), handle); err != nil || observation.State != condition.wantState || observation.Health != condition.wantHealth {
				t.Fatalf("observation for %+v: %+v, %v", condition.state, observation, err)
			}
		}
		manifest, err := adapter.Backup(t.Context(), handle)
		if err != nil || !slices.Equal(backups.backups, []CloudRunHandle{handle}) {
			t.Fatalf("backup hook: %+v, %v", manifest, err)
		}
		if err := adapter.Restore(t.Context(), handle, manifest); err != nil || !slices.Equal(backups.restores, []CloudRunBackupManifest{manifest}) {
			t.Fatalf("restore hook: %+v, %v", backups.restores, err)
		}
		proof, err := adapter.Decommission(t.Context(), handle)
		if err != nil || proof.Handle != handle || proof.ObservedAt.IsZero() {
			t.Fatalf("absence readback: %+v, %v", proof, err)
		}
		api.Lock()
		defer api.Unlock()
		if got := api.requests[len(api.requests)-3:]; !slices.Equal(got, []string{"GET " + target, "DELETE " + target, "GET " + target}) {
			t.Fatalf("decommission must verify exact target and GET after DELETE: %v", got)
		}
	})
	t.Run("foreign tenant and copied handle fail closed", func(t *testing.T) {
		api, makeAdapter := cloudRunTestSetup(t)
		store := &cloudRunTestR2{}
		adapterA, adapterB := makeAdapter("tenant-a", store), makeAdapter("tenant-b", store)
		handleA, err := adapterA.Provision(t.Context(), cloudRunTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		handleB, err := adapterB.Provision(t.Context(), cloudRunTestSpec())
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
		forgedService := handleA
		forgedService.ServiceName = handleB.ServiceName
		badLocation := handleA
		badLocation.Location = "bad/loc"
		emptyLocation := handleA
		emptyLocation.Location = ""
		forgedOwner := handleA
		forgedOwner.Owner = handleB.Owner
		forgedProject := handleA
		forgedProject.Project = "other-project-1"
		for _, handle := range []CloudRunHandle{handleB, forgedService, badLocation, emptyLocation, forgedOwner, forgedProject} {
			_, observeErr := adapterA.Observe(t.Context(), handle)
			_, backupErr := adapterA.Backup(t.Context(), handle)
			_, deleteErr := adapterA.Decommission(t.Context(), handle)
			checks := []error{
				adapterA.Start(t.Context(), handle), adapterA.Stop(t.Context(), handle), adapterA.Wake(t.Context(), handle),
				observeErr, backupErr, adapterA.Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: manifestB.Artifact}), deleteErr,
			}
			for _, err := range checks {
				if !errors.Is(err, ErrCloudRunOwnership) {
					t.Fatalf("expected ownership denial for %+v, got %v", handle, err)
				}
			}
		}
		if err := adapterA.Restore(t.Context(), handleA, manifestB); !errors.Is(err, ErrCloudRunOwnership) {
			t.Fatalf("foreign manifest: %v", err)
		}
		api.Lock()
		defer api.Unlock()
		if after := api.requests[before:]; len(after) != 0 || len(store.backups) != 1 || len(store.restores) != 0 {
			t.Fatalf("unexpected provider/R2 access %v, %+v", after, store)
		}
	})
	t.Run("provider-side label tampering fails closed before any mutation", func(t *testing.T) {
		api, makeAdapter := cloudRunTestSetup(t)
		store := &cloudRunTestR2{}
		adapter := makeAdapter("tenant-a", store)
		handle, err := adapter.Provision(t.Context(), cloudRunTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		api.Lock()
		service := api.services[cloudRunTestKey(CloudRunRegion, "svc-tenant-a")]
		labels := map[string]string{}
		for key, value := range service.Labels {
			labels[key] = value
		}
		labels["tenant_id"] = "tenant-b"
		service.Labels = labels
		api.services[cloudRunTestKey(CloudRunRegion, "svc-tenant-a")] = service
		before := len(api.requests)
		api.Unlock()
		_, observeErr := adapter.Observe(t.Context(), handle)
		_, backupErr := adapter.Backup(t.Context(), handle)
		_, deleteErr := adapter.Decommission(t.Context(), handle)
		for _, err := range []error{
			adapter.Start(t.Context(), handle), adapter.Stop(t.Context(), handle), adapter.Wake(t.Context(), handle),
			observeErr, backupErr, adapter.Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: CloudRunBackupArtifact{Ref: "r2-backup-1", SHA256: strings.Repeat("a", 64)}}), deleteErr,
		} {
			if !errors.Is(err, ErrCloudRunOwnership) {
				t.Fatalf("expected ownership denial for foreign labels, got %v", err)
			}
		}
		api.Lock()
		defer api.Unlock()
		for _, request := range api.requests[before:] {
			if !strings.HasPrefix(request, "GET ") {
				t.Fatalf("unauthorized side effect: %s", request)
			}
		}
		if len(store.backups) != 0 || len(store.restores) != 0 {
			t.Fatalf("R2 touched for a foreign service: %+v", store)
		}
	})
	t.Run("absence remains pending and only NOT FOUND releases evidence", func(t *testing.T) {
		for _, status := range []int{http.StatusNotFound, http.StatusOK} {
			api, makeAdapter := cloudRunTestSetup(t)
			adapter := makeAdapter("tenant-a", nil)
			handle, err := adapter.Provision(t.Context(), cloudRunTestSpec())
			if err != nil {
				t.Fatal(err)
			}
			api.Lock()
			api.delayDeletion = true
			api.deleteStatus = 0
			if status == http.StatusNotFound {
				api.deleteStatus = http.StatusNotFound
			}
			api.Unlock()
			if proof, err := adapter.Decommission(t.Context(), handle); !errors.Is(err, ErrCloudRunAbsencePending) || proof != (CloudRunAbsence{}) {
				t.Fatalf("DELETE %d without GET 404 cannot prove absence: %+v, %v", status, proof, err)
			}
			api.Lock()
			delete(api.services, cloudRunTestKey(CloudRunRegion, handle.ServiceName))
			before := len(api.requests)
			api.Unlock()
			if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle || proof.ObservedAt.IsZero() {
				t.Fatalf("decommission retry: %+v, %v", proof, err)
			}
			api.Lock()
			if len(api.requests) != before+1 || !strings.HasPrefix(api.requests[before], "GET ") {
				t.Fatalf("readback retry must make only a GET: %v", api.requests[before:])
			}
			api.Unlock()
		}
	})
	t.Run("readback drift blocks power-on without losing cleanup handle", func(t *testing.T) {
		raw := func(value string) json.RawMessage { return json.RawMessage(value) }
		zero := 0
		for index, drift := range []struct {
			name   string
			mutate func(*cloudRunService)
		}{
			{"warm instance pinned", func(s *cloudRunService) { s.Template.Scaling.MinInstanceCount = 1 }},
			{"service-level warm instance", func(s *cloudRunService) { s.Scaling = &cloudRunServiceScaling{MinInstanceCount: 1} }},
			{"manual instance count", func(s *cloudRunService) { s.Scaling = &cloudRunServiceScaling{ManualInstanceCount: &zero} }},
			{"unbounded scale", func(s *cloudRunService) { s.Template.Scaling.MaxInstanceCount = 100 }},
			{"unset max instances", func(s *cloudRunService) { s.Template.Scaling.MaxInstanceCount = 0 }},
			{"first generation sandbox", func(s *cloudRunService) { s.Template.ExecutionEnvironment = "EXECUTION_ENVIRONMENT_GEN1" }},
			{"unspecified sandbox", func(s *cloudRunService) { s.Template.ExecutionEnvironment = "" }},
			{"VPC access connector", func(s *cloudRunService) {
				s.Template.VPCAccess = raw(`{"connector":"projects/p/locations/europe-west3/connectors/c"}`)
			}},
			{"Cloud SQL volume", func(s *cloudRunService) {
				s.Template.Volumes = []json.RawMessage{raw(`{"name":"sql","cloudSqlInstance":{"instances":["p:europe-west3:db"]}}`)}
			}},
			{"GCS volume", func(s *cloudRunService) {
				s.Template.Volumes = []json.RawMessage{raw(`{"name":"data","gcs":{"bucket":"b"}}`)}
			}},
			{"volume mount", func(s *cloudRunService) {
				s.Template.Containers[0].VolumeMounts = []json.RawMessage{raw(`{"name":"data","mountPath":"/data"}`)}
			}},
			{"secret env binding", func(s *cloudRunService) {
				s.Template.Containers[0].Env = []cloudRunEnvVar{{Name: "KEY", ValueSource: raw(`{"secretKeyRef":{"secret":"s","version":"1"}}`)}}
			}},
			{"public ingress", func(s *cloudRunService) { s.Ingress = "INGRESS_TRAFFIC_ALL" }},
			{"empty ingress", func(s *cloudRunService) { s.Ingress = "" }},
			{"invoker IAM disabled", func(s *cloudRunService) { s.InvokerIAMDisabled = true }},
			{"multi-region settings", func(s *cloudRunService) { s.MultiRegionSettings = raw(`{"regions":["europe-west3","europe-west4"]}`) }},
			{"memory below Gen2 floor", func(s *cloudRunService) { s.Template.Containers[0].Resources.Limits["memory"] = "256Mi" }},
			{"memory above cap", func(s *cloudRunService) { s.Template.Containers[0].Resources.Limits["memory"] = "1Gi" }},
			{"cpu above cap", func(s *cloudRunService) { s.Template.Containers[0].Resources.Limits["cpu"] = "2000m" }},
			{"always allocated cpu", func(s *cloudRunService) { s.Template.Containers[0].Resources.CPUIdle = false }},
			{"extra limit", func(s *cloudRunService) { s.Template.Containers[0].Resources.Limits["nvidia.com/gpu"] = "1" }},
			{"sidecar container", func(s *cloudRunService) {
				s.Template.Containers = append(slices.Clone(s.Template.Containers), s.Template.Containers[0])
			}},
		} {
			api, makeAdapter := cloudRunTestSetup(t)
			store := &cloudRunTestR2{}
			adapter := makeAdapter(fmt.Sprintf("tenant-drift-%d", index), store)
			handle, err := adapter.Provision(t.Context(), cloudRunTestSpec())
			if err != nil {
				t.Fatal(err)
			}
			api.Lock()
			service := api.services[cloudRunTestKey(CloudRunRegion, handle.ServiceName)]
			service.Template.Containers = slices.Clone(service.Template.Containers)
			limits := map[string]string{}
			for key, value := range service.Template.Containers[0].Resources.Limits {
				limits[key] = value
			}
			service.Template.Containers[0].Resources.Limits = limits
			drift.mutate(&service)
			api.services[cloudRunTestKey(CloudRunRegion, handle.ServiceName)] = service
			api.Unlock()
			if err := adapter.Start(t.Context(), handle); !errors.Is(err, ErrCloudRunPolicyDrift) {
				t.Fatalf("%s: power-on with drift: %v", drift.name, err)
			}
			if err := adapter.Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: CloudRunBackupArtifact{Ref: "r2-backup-1", SHA256: strings.Repeat("a", 64)}}); !errors.Is(err, ErrCloudRunPolicyDrift) || len(store.restores) != 0 {
				t.Fatalf("%s: restore with drift: %v", drift.name, err)
			}
			observation, err := adapter.Observe(t.Context(), handle)
			if drift.name == "first generation sandbox" || drift.name == "unspecified sandbox" {
				if !errors.Is(err, ErrCloudRunIsolation) {
					t.Fatalf("%s: expected isolation denial, got %+v, %v", drift.name, observation, err)
				}
			} else if err != nil || observation.Health == CloudRunHealthHealthy {
				t.Fatalf("%s: drifted service must never observe healthy: %+v, %v", drift.name, observation, err)
			}
			if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle {
				t.Fatalf("%s: cleanup of drifted exact handle: %+v, %v", drift.name, proof, err)
			}
		}
	})
	t.Run("non-europe-west3 cleanup succeeds while active lifecycle, image and R2 keys enforce policy", func(t *testing.T) {
		api, makeAdapter := cloudRunTestSetup(t)
		backups := &cloudRunTestR2{}
		adapter := makeAdapter("tenant-a", backups)

		// 1a. A service resource whose name reports another location blocks active operations on readback.
		api.Lock()
		api.createLocation = "europe-west4"
		api.Unlock()
		handle, err := adapter.Provision(t.Context(), cloudRunTestSpec())
		var regionErr *CloudRunRegionError
		if !errors.As(err, &regionErr) || regionErr.Region != "europe-west4" || handle.ServiceName == "" {
			t.Fatalf("expected handle + europe-west4 region error on Provision, got handle=%+v err=%v", handle, err)
		}
		api.Lock()
		api.createLocation = ""
		api.Unlock()
		for _, blocked := range []error{
			adapter.Start(t.Context(), handle),
			adapter.Wake(t.Context(), handle),
			func() error { _, err := adapter.Observe(t.Context(), handle); return err }(),
			func() error { _, err := adapter.Backup(t.Context(), handle); return err }(),
			adapter.Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: CloudRunBackupArtifact{Ref: "tenants/tenant-a/workloads/workload-1/backups/snap-001.tar.zst", SHA256: strings.Repeat("a", 64)}}),
		} {
			var re *CloudRunRegionError
			if !errors.As(blocked, &re) || re.Region != "europe-west4" {
				t.Fatalf("non-europe-west3 service must fail active op with europe-west4 error, got %v", blocked)
			}
		}
		if len(backups.backups) != 0 || len(backups.restores) != 0 {
			t.Fatalf("R2 touched for a misplaced service: %+v", backups)
		}
		if err := adapter.Stop(t.Context(), handle); err != nil {
			t.Fatalf("Stop must clean up owned non-europe-west3 readback service, got %v", err)
		}
		if proof, err := adapter.Decommission(t.Context(), handle); err != nil || proof.Handle != handle {
			t.Fatalf("Decommission must clean up owned non-europe-west3 readback service, got %+v err=%v", proof, err)
		}

		// 1b. A handle stored at another Cloud Run region fails active ops locally and cleans up against locations/europe-west4.
		west4Handle := handle
		west4Handle.Location = "europe-west4"
		west4Service := cloudRunService{
			Name:    "projects/" + cloudRunTestProject + "/locations/europe-west4/services/" + west4Handle.ServiceName,
			Labels:  west4Handle.Owner.labels(),
			Ingress: cloudRunIngressInternalOnly,
			Template: cloudRunTemplate{
				Scaling:              cloudRunRevisionScaling{MinInstanceCount: 0, MaxInstanceCount: 1},
				ExecutionEnvironment: cloudRunExecutionGen2,
				Containers: []cloudRunContainer{{
					Image:     cloudRunTestImage,
					Ports:     []cloudRunPort{{ContainerPort: 8080}},
					Resources: cloudRunResources{Limits: map[string]string{"cpu": "1000m", "memory": "512Mi"}, CPUIdle: true},
				}},
			},
		}
		api.Lock()
		api.services[cloudRunTestKey("europe-west4", west4Handle.ServiceName)] = west4Service
		beforeWest4 := len(api.requests)
		api.Unlock()
		for _, blocked := range []error{
			adapter.Start(t.Context(), west4Handle),
			adapter.Wake(t.Context(), west4Handle),
			func() error { _, err := adapter.Observe(t.Context(), west4Handle); return err }(),
			func() error { _, err := adapter.Backup(t.Context(), west4Handle); return err }(),
			adapter.Restore(t.Context(), west4Handle, CloudRunBackupManifest{Source: west4Handle, Artifact: CloudRunBackupArtifact{Ref: "tenants/tenant-a/workloads/workload-1/backups/snap-001.tar.zst", SHA256: strings.Repeat("a", 64)}}),
		} {
			var re *CloudRunRegionError
			if !errors.As(blocked, &re) || re.Region != "europe-west4" {
				t.Fatalf("west4 handle active op must fail with europe-west4 region error, got %v", blocked)
			}
		}
		api.Lock()
		if len(api.requests) != beforeWest4 {
			t.Fatalf("west4 handle active ops must not call provider API: %v", api.requests[beforeWest4:])
		}
		api.Unlock()
		if err := adapter.Stop(t.Context(), west4Handle); err != nil {
			t.Fatalf("Stop must target locations/europe-west4 for west4 handle: %v", err)
		}
		if proof, err := adapter.Decommission(t.Context(), west4Handle); err != nil || proof.Handle != west4Handle {
			t.Fatalf("Decommission must delete at locations/europe-west4: %+v, %v", proof, err)
		}
		const west4Target = "/v2/projects/test-project-1/locations/europe-west4/services/svc-tenant-a"
		api.Lock()
		gotWest4 := slices.Clone(api.requests[beforeWest4:])
		_, stillInWest4 := api.services[cloudRunTestKey("europe-west4", west4Handle.ServiceName)]
		api.Unlock()
		if stillInWest4 || !slices.Equal(gotWest4, []string{"GET " + west4Target, "GET " + west4Target, "DELETE " + west4Target, "GET " + west4Target}) {
			t.Fatalf("regional Stop/Decommission must hit %s: still=%v reqs=%v", west4Target, stillInWest4, gotWest4)
		}

		// 2. Image drift on Provision readback returns the handle and ErrCloudRunPolicyDrift.
		api.Lock()
		api.tamperImage = "evil.example.com/other:latest"
		api.Unlock()
		handle2, err := makeAdapter("tenant-c", backups).Provision(t.Context(), cloudRunTestSpec())
		if !errors.Is(err, ErrCloudRunPolicyDrift) || handle2.ServiceName == "" {
			t.Fatalf("image drift must return handle + ErrCloudRunPolicyDrift, got handle=%+v err=%v", handle2, err)
		}

		// 3. Provision readback 404 maps to ErrCloudRunResponse, not ErrCloudRunNotFound, and keeps the handle.
		api.Lock()
		api.tamperImage = ""
		api.dropOnCreate = true
		api.Unlock()
		handle3, err := makeAdapter("tenant-d", backups).Provision(t.Context(), cloudRunTestSpec())
		if !errors.Is(err, ErrCloudRunResponse) || errors.Is(err, ErrCloudRunNotFound) || handle3.ServiceName == "" {
			t.Fatalf("Provision readback 404 must return handle + ErrCloudRunResponse, got handle=%+v err=%v", handle3, err)
		}

		// 4. A refused create (for example an existing service name) returns no handle and is never adopted.
		api.Lock()
		api.dropOnCreate = false
		api.createStatus = http.StatusConflict
		before := len(api.requests)
		api.Unlock()
		handle4, err := makeAdapter("tenant-e", backups).Provision(t.Context(), cloudRunTestSpec())
		var apiErr *CloudRunAPIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict || handle4 != (CloudRunHandle{}) {
			t.Fatalf("refused create must return no handle and the HTTP status, got handle=%+v err=%v", handle4, err)
		}
		api.Lock()
		if len(api.requests) != before+1 {
			t.Fatalf("a refused create must not be retried or adopted: %v", api.requests[before:])
		}
		api.Unlock()

		// 5. Realistic R2 object key syntax accepted; unsafe keys and malformed digests rejected.
		digest := strings.Repeat("a", 64)
		validRef := "tenants/tenant-a/workloads/workload-1/backups/snap-001.tar.zst"
		if !(CloudRunBackupArtifact{Ref: validRef, SHA256: digest}).valid() {
			t.Fatal("expected realistic R2 object key to satisfy CloudRunBackupArtifact.valid()")
		}
		for _, badRef := range []string{
			"",
			"../escape",
			"tenants/../escape",
			"/leading-slash",
			"tenants//double-slash",
			"tenants/trailing-slash/",
			"tenants\\backslash",
			"tenants/has space",
			" padded",
			"tenants/has\x00nul",
			"tenants/has\x1fcontrol",
			"tenants/has\x7fdel",
			strings.Repeat("a", 513),
		} {
			if (CloudRunBackupArtifact{Ref: badRef, SHA256: digest}).valid() {
				t.Fatalf("expected %q to fail CloudRunBackupArtifact.valid()", badRef)
			}
		}
		for _, badSHA := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64)} {
			if (CloudRunBackupArtifact{Ref: validRef, SHA256: badSHA}).valid() {
				t.Fatalf("expected digest %q to fail CloudRunBackupArtifact.valid()", badSHA)
			}
		}
	})
	t.Run("R2 custody failures never succeed", func(t *testing.T) {
		_, makeAdapter := cloudRunTestSetup(t)
		handle, err := makeAdapter("tenant-a", nil).Provision(t.Context(), cloudRunTestSpec())
		if err != nil {
			t.Fatal(err)
		}
		good := CloudRunBackupArtifact{Ref: "r2-backup-1", SHA256: strings.Repeat("a", 64)}
		for _, store := range []CloudRunR2Store{
			nil,
			cloudRunBadR2{err: errors.New("r2 down")},
			cloudRunBadR2{artifact: CloudRunBackupArtifact{Ref: "../escape", SHA256: good.SHA256}},
			cloudRunBadR2{artifact: CloudRunBackupArtifact{Ref: good.Ref, SHA256: "short"}},
		} {
			adapter := makeAdapter("tenant-a", store)
			if manifest, err := adapter.Backup(t.Context(), handle); !errors.Is(err, ErrCloudRunBackup) || manifest != (CloudRunBackupManifest{}) {
				t.Fatalf("backup with store %T: %+v, %v", store, manifest, err)
			}
		}
		for _, artifact := range []CloudRunBackupArtifact{{Ref: "../escape", SHA256: good.SHA256}, {Ref: good.Ref, SHA256: strings.Repeat("A", 64)}} {
			store := &cloudRunTestR2{}
			if err := makeAdapter("tenant-a", store).Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: artifact}); !errors.Is(err, ErrCloudRunBackup) || len(store.restores) != 0 {
				t.Fatalf("restore of invalid artifact %+v: %v", artifact, err)
			}
		}
		if err := makeAdapter("tenant-a", cloudRunBadR2{err: errors.New("r2 down")}).Restore(t.Context(), handle, CloudRunBackupManifest{Source: handle, Artifact: good}); !errors.Is(err, ErrCloudRunBackup) {
			t.Fatalf("restore through failing store: %v", err)
		}
	})
	t.Run("HTTP client refuses redirects, oversize bodies and never leaks the token", func(t *testing.T) {
		var leaked atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Add(1) }))
		t.Cleanup(target.Close)
		redirector := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			http.Redirect(writer, request, target.URL+"/v2/leak", http.StatusTemporaryRedirect)
		}))
		t.Cleanup(redirector.Close)
		oversize := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			name := "projects/" + cloudRunTestProject + "/locations/europe-west3/services/svc-tenant-a"
			_, _ = fmt.Fprintf(writer, `{"name":%q,"labels":{"padding":%q}}`, name, strings.Repeat("a", cloudRunResponseLimit))
		}))
		t.Cleanup(oversize.Close)
		wrongStatus := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{}`))
		}))
		t.Cleanup(wrongStatus.Close)
		handle := CloudRunHandle{Project: cloudRunTestProject, Location: CloudRunRegion, ServiceName: "svc-tenant-a", Owner: cloudRunTestOwner("tenant-a")}

		adapter, err := newCloudRunAdapter(cloudRunTestProject, "svc-tenant-a", cloudRunTestToken, cloudRunTestOwner("tenant-a"), nil, redirector.URL+"/v2")
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.Observe(t.Context(), handle)
		var apiErr *CloudRunAPIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusTemporaryRedirect || strings.Contains(err.Error(), cloudRunTestToken) {
			t.Fatalf("redirect must surface as an HTTP status error: %v", err)
		}
		if _, err := adapter.Decommission(t.Context(), handle); !errors.As(err, &apiErr) {
			t.Fatalf("redirected readback must not count as absence: %v", err)
		}
		if leaked.Load() != 0 {
			t.Fatalf("redirect was followed %d times", leaked.Load())
		}

		adapter, err = newCloudRunAdapter(cloudRunTestProject, "svc-tenant-a", cloudRunTestToken, cloudRunTestOwner("tenant-a"), nil, oversize.URL+"/v2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Observe(t.Context(), handle); !errors.Is(err, ErrCloudRunResponse) {
			t.Fatalf("oversize body must be refused: %v", err)
		}

		adapter, err = newCloudRunAdapter(cloudRunTestProject, "svc-tenant-a", cloudRunTestToken, cloudRunTestOwner("tenant-a"), nil, wrongStatus.URL+"/v2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Observe(t.Context(), handle); !errors.Is(err, ErrCloudRunResponse) {
			t.Fatalf("a decoded body behind a non-200 status must be refused: %v", err)
		}

		for _, formatted := range []string{
			fmt.Sprintf("%v", adapter), fmt.Sprintf("%+v", adapter), fmt.Sprintf("%#v", adapter), fmt.Sprint(adapter),
			fmt.Sprintf("%v", adapter.client), fmt.Sprintf("%+v", adapter.client), fmt.Sprintf("%#v", adapter.client), fmt.Sprint(adapter.client),
		} {
			if strings.Contains(formatted, cloudRunTestToken) {
				t.Fatalf("access token leaked through formatting: %s", formatted)
			}
		}

		closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		closedURL := closed.URL
		closed.Close()
		adapter, err = newCloudRunAdapter(cloudRunTestProject, "svc-tenant-a", cloudRunTestToken, cloudRunTestOwner("tenant-a"), nil, closedURL+"/v2")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := adapter.Observe(t.Context(), handle); !errors.Is(err, ErrCloudRunResponse) || strings.Contains(err.Error(), closedURL) || strings.Contains(err.Error(), cloudRunTestToken) {
			t.Fatalf("transport failure must not echo the URL or token: %v", err)
		}
	})
}
