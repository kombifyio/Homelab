package providercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type ionosTestIdentity struct{}

func ionosTenant(tenant string) context.Context {
	return context.WithValue(context.Background(), ionosTestIdentity{}, tenant)
}

type ionosTestAuth struct{ denied bool }

func (a *ionosTestAuth) AuthorizeIONOSSharedPool(ctx context.Context, c IONOSSharedPoolCommand) error {
	if a.denied || ctx.Value(ionosTestIdentity{}) != c.Spec.Workload.TenantID {
		return errors.New("denied")
	}
	return nil
}

type ionosTestCustody struct {
	mu      sync.Mutex
	records map[string]IONOSSharedPoolRecord
	failPut bool
}

func (s *ionosTestCustody) WithNode(ctx context.Context, _ string, fn func(IONOSSharedPoolLockedCustody) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(s)
}
func (s *ionosTestCustody) Records(context.Context) ([]IONOSSharedPoolRecord, error) {
	var out []IONOSSharedPoolRecord
	for _, v := range s.records {
		out = append(out, v)
	}
	return out, nil
}
func (s *ionosTestCustody) Put(_ context.Context, r IONOSSharedPoolRecord) error {
	if s.failPut {
		return errors.New("durability failed")
	}
	s.records[r.Spec.Workload.Handle] = r
	return nil
}

type ionosTestR2 struct {
	manifests map[string]IONOSSharedPoolBackupManifest
	deny      bool
}

func (b *ionosTestR2) VerifyIONOSSharedPoolBackup(_ context.Context, m IONOSSharedPoolBackupManifest) error {
	if b.deny || b.manifests[m.Key] != m {
		return errors.New("R2 object or authenticated manifest mismatch")
	}
	return nil
}

type ionosTestRuntime struct {
	objects                                                                           map[string]IONOSSharedPoolObservation
	r2                                                                                *ionosTestR2
	calls                                                                             []IONOSSharedPoolCommand
	foreignBackupKey                                                                  string
	failProvision, keepAfterDelete, leaveVolume, wrongOwner, wrongSpec, emptyEvidence bool
}

func (r *ionosTestRuntime) Execute(_ context.Context, c IONOSSharedPoolCommand) (IONOSSharedPoolObservation, error) {
	r.calls = append(r.calls, c)
	h := c.Spec.Workload.Handle
	o, ok := r.objects[h]
	if ok && o.Spec != c.Spec {
		return IONOSSharedPoolObservation{}, errors.New("agent ownership conflict")
	}
	if !ok {
		o = IONOSSharedPoolObservation{Spec: c.Spec, State: "absent", EvidenceRef: "agent/exact-readback", IsolationRemoved: true}
	}
	switch c.Action {
	case IONOSPoolProvision:
		if !ok {
			o.State = "stopped"
			o.IsolationRemoved = false
			r.objects[h] = o
		}
		if r.failProvision {
			return IONOSSharedPoolObservation{}, errors.New("response lost after create")
		}
	case IONOSPoolStart:
		o.State = "running"
		r.objects[h] = o
	case IONOSPoolStop, IONOSPoolRestore:
		o.State = "stopped"
		r.objects[h] = o
	case IONOSPoolBackup:
		key := c.Spec.VolumePrefix + "backups/snapshot"
		if r.foreignBackupKey != "" {
			key = r.foreignBackupKey
		}
		m := IONOSSharedPoolBackupManifest{Store: "r2", Bucket: "tenant-backups", Key: key, Version: "r2-version-1", SHA256: strings.Repeat("a", 64), SizeBytes: 128, Spec: c.Spec}
		r.r2.manifests[m.Key] = m
		o.Backup = &m
	case IONOSPoolDecommission:
		if !r.keepAfterDelete {
			o.State = "absent"
			o.IsolationRemoved = !r.leaveVolume
			r.objects[h] = o
		}
	}
	if r.wrongOwner {
		o.Spec.Workload.TenantID = "other-tenant"
	}
	if r.wrongSpec {
		o.Spec.NetworkNamespace = "host"
	}
	if r.emptyEvidence {
		o.EvidenceRef = ""
	}
	return o, nil
}
func ionosNode() IONOSSharedPoolNode {
	return IONOSSharedPoolNode{ID: "fra-worker-1", Location: "de/fra/2", MemoryMB: 32768, ReservedMemoryMB: 2048, Slots: 30}
}
func ionosWork(tenant, handle string) IONOSSharedPoolWorkload {
	return IONOSSharedPoolWorkload{TenantID: tenant, Handle: handle, Location: "de/fra/2", Image: "curated/vaultwarden@sha256:approved", MemoryMB: 512, Runtime: "gvisor"}
}
func ionosFixture(t *testing.T, n IONOSSharedPoolNode) (*IONOSSharedPoolAdapter, *ionosTestRuntime, *ionosTestCustody, *ionosTestAuth) {
	t.Helper()
	b := &ionosTestR2{manifests: map[string]IONOSSharedPoolBackupManifest{}}
	r := &ionosTestRuntime{objects: map[string]IONOSSharedPoolObservation{}, r2: b}
	s := &ionosTestCustody{records: map[string]IONOSSharedPoolRecord{}}
	auth := &ionosTestAuth{}
	a, err := NewIONOSSharedPoolAdapter(n, r, s, auth, b)
	if err != nil {
		t.Fatal(err)
	}
	return a, r, s, auth
}
func ionosRefused(t *testing.T, err error, code string) {
	t.Helper()
	var refusal *IONOSSharedPoolRefusal
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("want %s refusal, got %v", code, err)
	}
}
func ionosProvision(t *testing.T, a *IONOSSharedPoolAdapter, w IONOSSharedPoolWorkload) IONOSSharedPoolObservation {
	t.Helper()
	o, err := a.Provision(ionosTenant(w.TenantID), w)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func TestIONOSSharedPoolAdmission(t *testing.T) {
	for _, location := range []string{"", "fra", "us/las", "de/fra/1", "de/fra/2 "} {
		t.Run("node-location-"+location, func(t *testing.T) {
			n := ionosNode()
			n.Location = location
			_, err := NewIONOSSharedPoolAdapter(n, nil, nil, nil, nil)
			ionosRefused(t, err, "location_refused")
		})
	}
	for _, runtime := range []string{"gvisor", "runsc", "incus"} {
		t.Run(runtime, func(t *testing.T) {
			a, _, _, _ := ionosFixture(t, ionosNode())
			w := ionosWork("tenant-a", "one")
			w.Runtime = runtime
			o := ionosProvision(t, a, w)
			expected := "runsc"
			expectedRuntime := "gvisor"
			if runtime == "incus" {
				expected = "incus"
				expectedRuntime = "incus"
			}
			if o.Spec.RuntimeBinary != expected || o.Spec.Workload.Runtime != expectedRuntime {
				t.Fatal(o.Spec)
			}
		})
	}
	cases := []struct {
		name, code string
		change     func(*IONOSSharedPoolWorkload)
	}{
		{"empty-location", "location_refused", func(w *IONOSSharedPoolWorkload) { w.Location = "" }},
		{"other-location", "location_refused", func(w *IONOSSharedPoolWorkload) { w.Location = "us/las" }},
		{"socket", "docker_socket_refused", func(w *IONOSSharedPoolWorkload) { w.RequiresDockerSocket = true }},
		{"513-MB", "workload_memory_refused", func(w *IONOSSharedPoolWorkload) { w.MemoryMB = 513 }},
		{"zero-MB", "workload_memory_refused", func(w *IONOSSharedPoolWorkload) { w.MemoryMB = 0 }},
		{"path-handle", "identity_refused", func(w *IONOSSharedPoolWorkload) { w.Handle = "../other" }},
	}
	for _, runtime := range []string{"docker", "shared-docker", "runc", "", "unknown", "GVisor"} {
		cases = append(cases, struct {
			name, code string
			change     func(*IONOSSharedPoolWorkload)
		}{"runtime-" + runtime, "isolation_runtime_refused", func(w *IONOSSharedPoolWorkload) { w.Runtime = runtime }})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, r, s, _ := ionosFixture(t, ionosNode())
			w := ionosWork("tenant-a", "one")
			tc.change(&w)
			_, err := a.Provision(ionosTenant("tenant-a"), w)
			ionosRefused(t, err, tc.code)
			if len(r.calls) != 0 || len(s.records) != 0 {
				t.Fatal("denial dispatched or reserved")
			}
		})
	}
	for _, slots := range []int{29, 41} {
		n := ionosNode()
		n.Slots = slots
		_, err := NewIONOSSharedPoolAdapter(n, nil, nil, nil, nil)
		ionosRefused(t, err, "node_density_refused")
	}
	for _, memory := range []int{32767, 65537} {
		n := ionosNode()
		n.MemoryMB = memory
		_, err := NewIONOSSharedPoolAdapter(n, nil, nil, nil, nil)
		ionosRefused(t, err, "node_memory_refused")
	}
	for _, orchestrator := range []string{"kubernetes", "k3s", "talos"} {
		n := ionosNode()
		n.Orchestrator = orchestrator
		_, err := NewIONOSSharedPoolAdapter(n, nil, nil, nil, nil)
		ionosRefused(t, err, "orchestrator_refused")
	}
}

func TestIONOSSharedPoolTenantIsolationLifecycleAndR2(t *testing.T) {
	a, r, _, auth := ionosFixture(t, ionosNode())
	alice := ionosProvision(t, a, ionosWork("tenant-a", "alice"))
	bob := ionosProvision(t, a, ionosWork("tenant-b", "bob"))
	if alice.Spec.Schema == bob.Spec.Schema || alice.Spec.VolumePrefix == bob.Spec.VolumePrefix || alice.Spec.NetworkNamespace == bob.Spec.NetworkNamespace || alice.Spec.TenantLabel == bob.Spec.TenantLabel {
		t.Fatal("shared tenant isolation")
	}
	// Same tenant, separate app: deleting one must not remove another's schema.
	alice2 := ionosProvision(t, a, ionosWork("tenant-a", "alice-two"))
	if alice.Spec.Schema == alice2.Spec.Schema {
		t.Fatal("shared workload schema")
	}
	ops := []struct {
		name string
		fn   func(context.Context, string, string) (IONOSSharedPoolObservation, error)
	}{{"start", a.Start}, {"stop", a.Stop}, {"observe", a.Observe}, {"backup", a.Backup}, {"decommission", a.Decommission}}
	for _, op := range ops {
		t.Run("cross-tenant-"+op.name, func(t *testing.T) {
			before := len(r.calls)
			_, err := op.fn(ionosTenant("tenant-a"), "tenant-a", "bob")
			ionosRefused(t, err, "ownership_refused")
			_, err = op.fn(ionosTenant("tenant-a"), "tenant-b", "bob")
			ionosRefused(t, err, "authorization_refused")
			if len(r.calls) != before {
				t.Fatal("cross-tenant agent access")
			}
		})
	}
	_, err := a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "bob"))
	ionosRefused(t, err, "ownership_refused")
	started, err := a.Start(ionosTenant("tenant-a"), "tenant-a", "alice")
	if err != nil || started.State != "running" {
		t.Fatal(started, err)
	}
	// Idempotent provision preserves a running generation and does not duplicate capacity.
	replay := ionosProvision(t, a, ionosWork("tenant-a", "alice"))
	if replay.State != "running" {
		t.Fatal(replay)
	}
	stopped, err := a.Stop(ionosTenant("tenant-a"), "tenant-a", "alice")
	if err != nil || stopped.State != "stopped" {
		t.Fatal(stopped, err)
	}
	backup, err := a.Backup(ionosTenant("tenant-a"), "tenant-a", "alice")
	if err != nil || backup.Backup == nil {
		t.Fatal(backup, err)
	}
	before := len(r.calls)
	_, err = a.Restore(ionosTenant("tenant-b"), "tenant-b", "bob", *backup.Backup)
	ionosRefused(t, err, "backup_refused")
	_, err = a.Restore(ionosTenant("tenant-a"), "tenant-a", "alice-two", *backup.Backup)
	ionosRefused(t, err, "backup_refused")
	_, err = a.Restore(ionosTenant("tenant-a"), "tenant-a", "bob", *backup.Backup)
	ionosRefused(t, err, "ownership_refused")
	if len(r.calls) != before {
		t.Fatal("cross-handle restore reached agent")
	}
	restored, err := a.Restore(ionosTenant("tenant-a"), "tenant-a", "alice", *backup.Backup)
	if err != nil || restored.State != "stopped" {
		t.Fatal(restored, err)
	}
	mutations := []struct {
		name           string
		registerInFake bool
		mutate         func(*IONOSSharedPoolBackupManifest)
	}{
		{"foreign-store", false, func(m *IONOSSharedPoolBackupManifest) { m.Store = "s3" }},
		{"empty-bucket", true, func(m *IONOSSharedPoolBackupManifest) { m.Bucket = "" }},
		{"invalid-sha256", false, func(m *IONOSSharedPoolBackupManifest) { m.SHA256 = "invalid" }},
		{"other-version", false, func(m *IONOSSharedPoolBackupManifest) { m.Version = "other" }},
		{"foreign-prefix-key", true, func(m *IONOSSharedPoolBackupManifest) { m.Key = bob.Spec.VolumePrefix + "backups/snapshot" }},
		{"dotdot-key", true, func(m *IONOSSharedPoolBackupManifest) { m.Key += "/../other" }},
		{"backslash-key", true, func(m *IONOSSharedPoolBackupManifest) { m.Key += `\other` }},
		{"nul-key", true, func(m *IONOSSharedPoolBackupManifest) { m.Key += "\x00other" }},
		{"zero-size", false, func(m *IONOSSharedPoolBackupManifest) { m.SizeBytes = 0 }},
	}
	for _, tc := range mutations {
		t.Run("restore-rejects-"+tc.name, func(t *testing.T) {
			m := *backup.Backup
			tc.mutate(&m)
			if tc.registerInFake {
				r.r2.manifests[m.Key] = m
			}
			callsBefore := len(r.calls)
			_, err = a.Restore(ionosTenant("tenant-a"), "tenant-a", "alice", m)
			ionosRefused(t, err, "backup_refused")
			if len(r.calls) != callsBefore {
				t.Fatalf("%s reached agent unexpectedly", tc.name)
			}
		})
	}
	r.foreignBackupKey = bob.Spec.VolumePrefix + "backups/forged"
	_, err = a.Backup(ionosTenant("tenant-a"), "tenant-a", "alice")
	ionosRefused(t, err, "backup_refused")
	r.foreignBackupKey = ""
	r.r2.deny = true
	_, err = a.Backup(ionosTenant("tenant-a"), "tenant-a", "alice")
	ionosRefused(t, err, "backup_refused")
	auth.denied = true
	before = len(r.calls)
	_, err = a.Start(ionosTenant("tenant-a"), "tenant-a", "alice")
	ionosRefused(t, err, "authorization_refused")
	if len(r.calls) != before {
		t.Fatal("revoked authority dispatched")
	}
}

func TestIONOSSharedPoolCapacityAndExactDecommission(t *testing.T) {
	for _, slots := range []int{30, 40} {
		t.Run(fmt.Sprint(slots), func(t *testing.T) {
			n := ionosNode()
			n.Slots = slots
			a, r, s, _ := ionosFixture(t, n)
			for i := 0; i < slots; i++ {
				ionosProvision(t, a, ionosWork("tenant-a", fmt.Sprintf("app-%d", i)))
			}
			_, err := a.Provision(ionosTenant("tenant-b"), ionosWork("tenant-b", "overflow"))
			ionosRefused(t, err, "capacity_refused")
			r.keepAfterDelete = true
			_, err = a.Decommission(ionosTenant("tenant-a"), "tenant-a", "app-0")
			ionosRefused(t, err, "absence_unconfirmed")
			if s.records["app-0"].Released {
				t.Fatal("released present resource")
			}
			r.keepAfterDelete = false
			r.leaveVolume = true
			_, err = a.Decommission(ionosTenant("tenant-a"), "tenant-a", "app-0")
			ionosRefused(t, err, "absence_unconfirmed")
			r.leaveVolume = false
			beforeDecom := len(r.calls)
			o, err := a.Decommission(ionosTenant("tenant-a"), "tenant-a", "app-0")
			if err != nil || o.State != "absent" || !s.records["app-0"].Released {
				t.Fatal(o, err)
			}
			for _, call := range r.calls[beforeDecom:] {
				if call.Spec.Workload.Handle != "app-0" || call.Spec.Workload.TenantID != "tenant-a" {
					t.Fatalf("decommission targeted wrong workload: %+v", call.Spec.Workload)
				}
			}
			if r.objects["app-1"].State != "stopped" {
				t.Fatal("removed neighboring workload")
			}
			ionosProvision(t, a, ionosWork("tenant-b", "replacement"))
			_, err = a.Decommission(ionosTenant("tenant-a"), "tenant-a", "app-0")
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "overflow-again"))
			ionosRefused(t, err, "capacity_refused")
			_, err = a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "app-0"))
			ionosRefused(t, err, "generation_released")
		})
	}
	t.Run("RAM", func(t *testing.T) {
		n := ionosNode()
		n.ReservedMemoryMB = n.MemoryMB - 512
		a, _, _, _ := ionosFixture(t, n)
		ionosProvision(t, a, ionosWork("tenant-a", "one"))
		_, err := a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "two"))
		ionosRefused(t, err, "capacity_refused")
		if _, err = a.Decommission(ionosTenant("tenant-a"), "tenant-a", "one"); err != nil {
			t.Fatal(err)
		}
		ionosProvision(t, a, ionosWork("tenant-a", "two"))
	})
}

func TestIONOSSharedPoolAmbiguityAndReadback(t *testing.T) {
	n := ionosNode()
	n.ReservedMemoryMB = n.MemoryMB - 512
	a, r, s, auth := ionosFixture(t, n)
	r.failProvision = true
	_, err := a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "one"))
	if err == nil {
		t.Fatal("lost response succeeded")
	}
	// Reconstructing the adapter uses persisted reservations, never empty capacity.
	restarted, err := NewIONOSSharedPoolAdapter(n, r, s, auth, r.r2)
	if err != nil {
		t.Fatal(err)
	}
	_, err = restarted.Provision(ionosTenant("tenant-b"), ionosWork("tenant-b", "two"))
	ionosRefused(t, err, "capacity_refused")
	r.failProvision = false
	ionosProvision(t, restarted, ionosWork("tenant-a", "one"))
	if len(s.records) != 1 {
		t.Fatal("duplicate reservation")
	}
	for _, tamper := range []*bool{&r.wrongOwner, &r.wrongSpec, &r.emptyEvidence} {
		*tamper = true
		before := len(r.calls)
		_, err = a.Start(ionosTenant("tenant-a"), "tenant-a", "one")
		ionosRefused(t, err, "readback_refused")
		if len(r.calls) != before+1 || r.calls[len(r.calls)-1].Action != IONOSPoolObserve {
			t.Fatal("mutated after invalid pre-read")
		}
		*tamper = false
	}
	a2, r2, s2, _ := ionosFixture(t, ionosNode())
	s2.failPut = true
	_, err = a2.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", "one"))
	if err == nil || len(r2.calls) != 0 {
		t.Fatal("dispatched before durable reservation")
	}
}

func TestIONOSSharedPoolConcurrentAdmission(t *testing.T) {
	a, _, s, _ := ionosFixture(t, ionosNode())
	var wg sync.WaitGroup
	var mu sync.Mutex
	successes := 0
	for i := 0; i < 45; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := a.Provision(ionosTenant("tenant-a"), ionosWork("tenant-a", fmt.Sprintf("app-%d", i)))
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				successes++
			} else {
				var refusal *IONOSSharedPoolRefusal
				if !errors.As(err, &refusal) || refusal.Code != "capacity_refused" {
					t.Errorf("want capacity_refused refusal, got %v", err)
				}
			}
		}(i)
	}
	wg.Wait()
	if successes != 30 || len(s.records) != 30 {
		t.Fatalf("capacity overbooked: %d/%d", successes, len(s.records))
	}
}

func TestIONOSSharedPoolHTTPSAgent(t *testing.T) {
	a, r, _, _ := ionosFixture(t, ionosNode())
	spec := ionosProvision(t, a, ionosWork("tenant-a", "one")).Spec
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v1/ionos-shared-pool/execute" || req.Method != "POST" {
			t.Error("wrong agent endpoint")
		}
		var c IONOSSharedPoolCommand
		if err := json.NewDecoder(req.Body).Decode(&c); err != nil {
			t.Error(err)
		}
		o, err := r.Execute(req.Context(), c)
		if err != nil {
			t.Error(err)
		}
		_ = json.NewEncoder(w).Encode(o)
	}))
	defer server.Close()
	client, err := NewIONOSSharedPoolHTTPRuntime(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	o, err := client.Execute(context.Background(), IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec})
	if err != nil || o.Spec != spec || o.State != "stopped" {
		t.Fatal(o, err)
	}
	for _, origin := range []string{"http://worker", "https://user:password@worker", "https://worker/?token=secret", "https://worker/other"} {
		_, err := NewIONOSSharedPoolHTTPRuntime(origin, server.Client())
		ionosRefused(t, err, "agent_endpoint_refused")
	}
	for _, body := range []string{`{}`, `{"state":"running","extra":"unknown"}`, `{} {}`, strings.Repeat("x", (1<<20)+1)} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
		t.Cleanup(srv.Close)
		c, err := NewIONOSSharedPoolHTTPRuntime(srv.URL, srv.Client())
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.Execute(context.Background(), IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec})
		if err == nil {
			t.Fatal("unsafe response accepted")
		}
	}
	var redirected atomic.Bool
	dest := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Store(true) }))
	defer dest.Close()
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dest.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	c, err := NewIONOSSharedPoolHTTPRuntime(redirect.URL, redirect.Client())
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Execute(context.Background(), IONOSSharedPoolCommand{Action: IONOSPoolStart, Spec: spec})
	if err == nil || redirected.Load() {
		t.Fatal("followed mutation redirect")
	}
}
