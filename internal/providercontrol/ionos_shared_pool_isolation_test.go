package providercontrol

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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

func TestIONOSSharedPoolHostProfile(t *testing.T) {
	for _, memory := range []int{32 * 1024, 64 * 1024} {
		t.Run(fmt.Sprint(memory), func(t *testing.T) {
			node := ionosNode()
			node.MemoryMB = memory
			profile, err := BuildIONOSSharedWorkerProfile(node)
			if err != nil {
				t.Fatal(err)
			}
			again, err := BuildIONOSSharedWorkerProfile(node)
			if err != nil || again.CloudInit != profile.CloudInit || again.CloudInitSHA256 != profile.CloudInitSHA256 || len(profile.CloudInitSHA256) != 64 {
				t.Fatal("profile is not deterministic")
			}
			if profile.NodeID != node.ID || profile.Location != "de/fra/2" || profile.MemoryMB != memory || profile.Slots != node.Slots || profile.BackupStore != "r2" {
				t.Fatalf("profile does not describe the admitted node: %+v", profile)
			}
			if strings.Join(profile.IsolationRuntimes, ",") != "gvisor,incus" || strings.Join(profile.MaskedUnits, ",") != "docker.service,docker.socket" {
				t.Fatalf("runtimes or masked units: %+v", profile)
			}
			agent := profile.Agent
			if agent.Path != "/v1/ionos-shared-pool/execute" || agent.Path != IONOSSharedPoolAgentPath || !agent.TLSRequired || !agent.ClientCertRequired || agent.Interface != IONOSSharedWorkerAgentInterface || agent.AllowedSourceCIDR != IONOSSharedWorkerControlPlaneCIDR || agent.Port != IONOSSharedWorkerAgentPort {
				t.Fatalf("agent listener: %+v", agent)
			}
			script := profile.CloudInit
			for _, want := range []string{
				"#!/bin/bash", "set -euo pipefail", node.ID, "runsc", "incus", "apt-mark hold runsc incus",
				"systemctl mask docker.service docker.socket", "Pin-Priority: -1",
				"br_netfilter", "net.bridge.bridge-nf-call-iptables = 1", "table bridge kombify_shared_worker_bridge",
				`iifname { "kw*", "incusbr*" } drop`,
				`iifname { "kw*", "incusbr*" } oifname { "kw*", "incusbr*" } drop`,
				`iifname "wg-kombify" ip saddr 100.64.200.0/24 tcp dport 8443 accept`,
				`include "/etc/kombify/ionos-shared-worker/nft.d/*.nft"`,
				"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16", "100.64.0.0/10", "fc00::/7", "fe80::/10",
				"policy drop", "table inet @NETNS@", "net-<sha256",
				"listen_addresses = '127.0.0.1'", "tenant_<sha256(tenant)[:24]>_app_<sha256(tenant NUL handle)[:24]>",
				`GRANT CONNECT ON DATABASE kombify_shared TO "@SCHEMA@";`,
				"REVOKE ALL ON SCHEMA public FROM PUBLIC", "KOMBIFY_BACKUP_STORE=r2", "KOMBIFY_BACKUP_VERIFY=version,size,sha256",
				"KOMBIFY_AGENT_TLS=required", "KOMBIFY_AGENT_CLIENT_AUTH=require-and-verify", "KOMBIFY_AGENT_PATH=" + IONOSSharedPoolAgentPath,
				"KOMBIFY_AGENT_INTERFACE=wg-kombify", "KOMBIFY_AGENT_ALLOWED_CIDR=100.64.200.0/24",
				"KOMBIFY_AGENT_PORT=8443", "KOMBIFY_AGENT_MAX_BODY_BYTES=1048576",
				"/etc/kombify/ionos-shared-worker/start-agent.sh", "ConditionPathExists=/sys/class/net/wg-kombify",
			} {
				if !strings.Contains(script, want) {
					t.Errorf("cloud-init lacks %q", want)
				}
			}
			for _, unwanted := range []string{"\ttcp dport 8443 accept", "private-interface"} {
				if strings.Contains(script, unwanted) {
					t.Errorf("cloud-init must not contain %q", unwanted)
				}
			}
			lower := strings.ToLower(script)
			for _, forbidden := range []string{"k3s", "k8s", "talos", "kubernetes", "secret", "token", "begin ", "private key", "akia", "access_key", "password =", "@@"} {
				if strings.Contains(lower, forbidden) {
					t.Errorf("cloud-init contains forbidden %q", forbidden)
				}
			}
			for _, line := range strings.Split(script, "\n") {
				if strings.Contains(line, "apt-get install") && strings.Contains(strings.ToLower(line), "docker") {
					t.Errorf("cloud-init installs Docker: %q", line)
				}
			}
		})
	}
	cases := []struct {
		name, code string
		change     func(*IONOSSharedPoolNode)
	}{
		{"location", "location_refused", func(n *IONOSSharedPoolNode) { n.Location = "de/fra/1" }},
		{"below-32-GB", "node_memory_refused", func(n *IONOSSharedPoolNode) { n.MemoryMB = 32767 }},
		{"above-64-GB", "node_memory_refused", func(n *IONOSSharedPoolNode) { n.MemoryMB = 65537 }},
		{"slots", "node_density_refused", func(n *IONOSSharedPoolNode) { n.Slots = 41 }},
		{"kubernetes", "orchestrator_refused", func(n *IONOSSharedPoolNode) { n.Orchestrator = "kubernetes" }},
		{"k3s", "orchestrator_refused", func(n *IONOSSharedPoolNode) { n.Orchestrator = "k3s" }},
		{"talos", "orchestrator_refused", func(n *IONOSSharedPoolNode) { n.Orchestrator = "talos" }},
		{"node-id", "node_refused", func(n *IONOSSharedPoolNode) { n.ID = "worker;rm -rf /" }},
	}
	for _, tc := range cases {
		t.Run("refuses-"+tc.name, func(t *testing.T) {
			node := ionosNode()
			tc.change(&node)
			profile, err := BuildIONOSSharedWorkerProfile(node)
			ionosRefused(t, err, tc.code)
			if profile.CloudInit != "" {
				t.Fatal("refused node rendered a script")
			}
		})
	}
}

type ionosTestMTLSFixture struct {
	serverTLSConfig *tls.Config
	trustedClient   *http.Client
	noCertClient    *http.Client
	rogueClient     *http.Client
	clientLeaf      *x509.Certificate
	caLeaf          *x509.Certificate
	serverCert      tls.Certificate
	caPEM           []byte
}

func ionosTestMTLS(t *testing.T) ionosTestMTLSFixture {
	t.Helper()
	gen := func(tmpl *x509.Certificate, signer *x509.Certificate, signerKey *ecdsa.PrivateKey) (tls.Certificate, *x509.Certificate, *ecdsa.PrivateKey, []byte) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		parent, parentKey := tmpl, key
		if signer != nil {
			parent, parentKey = signer, signerKey
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
		if err != nil {
			t.Fatal(err)
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
		tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return tlsCert, leaf, key, certPEM
	}
	now := time.Now().Add(-time.Minute)
	exp := time.Now().Add(time.Hour)
	_, caLeaf, caKey, caPEM := gen(&x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kombify-test-ca"},
		NotBefore: now, NotAfter: exp, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil, nil)
	serverCert, _, _, _ := gen(&x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now, NotAfter: exp, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, caLeaf, caKey)
	clientCert, clientLeaf, _, _ := gen(&x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "control-plane"},
		NotBefore: now, NotAfter: exp,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, caLeaf, caKey)
	_, rogueLeaf, rogueKey, _ := gen(&x509.Certificate{
		SerialNumber: big.NewInt(4), Subject: pkix.Name{CommonName: "rogue-ca"},
		NotBefore: now, NotAfter: exp, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}, nil, nil)
	rogueCert, _, _, _ := gen(&x509.Certificate{
		SerialNumber: big.NewInt(5), Subject: pkix.Name{CommonName: "rogue-client"},
		NotBefore: now, NotAfter: exp,
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}, rogueLeaf, rogueKey)

	serverTLS, err := NewIONOSSharedPoolAgentTLSConfig(serverCert, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("append root CA")
	}
	return ionosTestMTLSFixture{
		serverTLSConfig: serverTLS,
		trustedClient:   &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{clientCert}, MinVersion: tls.VersionTLS13}}},
		noCertClient:    &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13}}},
		rogueClient:     &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{rogueCert}, MinVersion: tls.VersionTLS13}}},
		clientLeaf:      clientLeaf,
		caLeaf:          caLeaf,
		serverCert:      serverCert,
		caPEM:           caPEM,
	}
}

func TestIONOSSharedPoolAgentHandler(t *testing.T) {
	mtls := ionosTestMTLS(t)
	if mtls.serverTLSConfig.MinVersion != tls.VersionTLS13 || mtls.serverTLSConfig.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("server TLS posture not hardened: %+v", mtls.serverTLSConfig)
	}
	if _, err := NewIONOSSharedPoolAgentTLSConfig(tls.Certificate{}, mtls.caPEM); err == nil {
		t.Fatal("empty server cert accepted")
	}
	if _, err := NewIONOSSharedPoolAgentTLSConfig(mtls.serverCert, []byte("not-pem")); err == nil {
		t.Fatal("invalid client CA accepted")
	}

	node := ionosNode()
	a, r, s, auth := ionosFixture(t, node)
	_ = a
	server := httptest.NewUnstartedServer(NewIONOSSharedPoolAgentHandler(node, r))
	server.TLS = mtls.serverTLSConfig
	server.StartTLS()
	defer server.Close()
	client, err := NewIONOSSharedPoolHTTPRuntime(server.URL, mtls.trustedClient)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := NewIONOSSharedPoolAdapter(node, client, s, auth, r.r2)
	if err != nil {
		t.Fatal(err)
	}
	work := ionosWork("tenant-a", "one")
	ctx := ionosTenant("tenant-a")
	provisioned, err := adapter.Provision(ctx, work)
	if err != nil || provisioned.State != "stopped" {
		t.Fatal(provisioned, err)
	}
	spec := provisioned.Spec
	started, err := adapter.Start(ctx, "tenant-a", "one")
	if err != nil || started.State != "running" {
		t.Fatal(started, err)
	}
	backup, err := adapter.Backup(ctx, "tenant-a", "one")
	if err != nil || backup.Backup == nil {
		t.Fatal(backup, err)
	}
	restored, err := adapter.Restore(ctx, "tenant-a", "one", *backup.Backup)
	if err != nil || restored.State != "stopped" {
		t.Fatal(restored, err)
	}
	removed, err := adapter.Decommission(ctx, "tenant-a", "one")
	if err != nil || removed.State != "absent" || !removed.IsolationRemoved || !s.records["one"].Released {
		t.Fatal(removed, err)
	}

	// TLS peers without a trusted client certificate fail the mTLS handshake.
	for name, badHTTPClient := range map[string]*http.Client{
		"no-client-cert": mtls.noCertClient,
		"rogue-ca-cert":  mtls.rogueClient,
	} {
		t.Run("mtls-rejects-"+name, func(t *testing.T) {
			untrusted, err := NewIONOSSharedPoolHTTPRuntime(server.URL, badHTTPClient)
			if err != nil {
				t.Fatal(err)
			}
			before := len(r.calls)
			if _, err := untrusted.Execute(context.Background(), IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec}); err == nil {
				t.Fatal("untrusted TLS peer reached agent")
			}
			if len(r.calls) != before {
				t.Fatal("untrusted TLS peer invoked runtime")
			}
		})
	}

	// The agent derives every isolation field itself and never trusts the caller's.
	manifest := IONOSSharedPoolBackupManifest{Store: "r2", Bucket: "tenant-backups", Key: spec.VolumePrefix + "backups/snapshot", Version: "r2-version-1", SHA256: strings.Repeat("a", 64), SizeBytes: 128, Spec: spec}
	rejected := []struct {
		name   string
		action IONOSSharedPoolAction
		change func(*IONOSSharedPoolCommand)
	}{
		{"network-namespace", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.NetworkNamespace = "host" }},
		{"schema", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.Schema = "public" }},
		{"volume-prefix", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.VolumePrefix = "tenants/other/" }},
		{"runtime-binary", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.RuntimeBinary = "runc" }},
		{"tenant-label", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.TenantLabel = "tenant-b" }},
		{"node-id", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.NodeID = "other-worker" }},
		{"docker-socket", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.Workload.RequiresDockerSocket = true }},
		{"memory", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.Workload.MemoryMB = 1024 }},
		{"location", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.Workload.Location = "us/las" }},
		{"runtime", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Spec.Workload.Runtime = "docker" }},
		{"unknown-action", "exec", func(*IONOSSharedPoolCommand) {}},
		{"backup-on-start", IONOSPoolStart, func(c *IONOSSharedPoolCommand) { c.Backup = &manifest }},
		{"restore-without-backup", IONOSPoolRestore, func(*IONOSSharedPoolCommand) {}},
		{"restore-foreign-store", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.Store = "s3"; c.Backup = &m }},
		{"restore-foreign-prefix", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.Key = "tenants/other/backups/snapshot"; c.Backup = &m }},
		{"restore-dotdot-key", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.Key += "/../other"; c.Backup = &m }},
		{"restore-zero-size", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.SizeBytes = 0; c.Backup = &m }},
		{"restore-bad-sha256", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.SHA256 = "invalid"; c.Backup = &m }},
		{"restore-foreign-spec", IONOSPoolRestore, func(c *IONOSSharedPoolCommand) { m := manifest; m.Spec.Schema = "public"; c.Backup = &m }},
	}
	for _, tc := range rejected {
		t.Run("rejects-"+tc.name, func(t *testing.T) {
			command := IONOSSharedPoolCommand{Action: tc.action, Spec: spec}
			tc.change(&command)
			before := len(r.calls)
			if _, err := client.Execute(context.Background(), command); err == nil {
				t.Fatal("tampered command accepted")
			}
			if len(r.calls) != before {
				t.Fatal("tampered command reached the runtime")
			}
		})
	}

	serve := func(handler http.Handler, method, path, body string, secure, verifiedClient bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if secure {
			state := &tls.ConnectionState{}
			if verifiedClient {
				state.PeerCertificates = []*x509.Certificate{mtls.clientLeaf}
				state.VerifiedChains = [][]*x509.Certificate{{mtls.clientLeaf, mtls.caLeaf}}
			}
			req.TLS = state
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}
	handler := NewIONOSSharedPoolAgentHandler(node, r)
	valid, err := json.Marshal(IONOSSharedPoolCommand{Action: IONOSPoolObserve, Spec: spec})
	if err != nil {
		t.Fatal(err)
	}
	before := len(r.calls)
	if rec := serve(handler, http.MethodPost, IONOSSharedPoolAgentPath, string(valid), true, true); rec.Code != http.StatusOK {
		t.Fatalf("valid observe: %d %s", rec.Code, rec.Body)
	}
	if len(r.calls) != before+1 {
		t.Fatal("valid command did not reach the runtime exactly once")
	}
	before = len(r.calls)
	for _, tc := range []struct {
		name, method, path, body string
		secure, verifiedClient   bool
		status                   int
	}{
		{"wrong-method", http.MethodGet, IONOSSharedPoolAgentPath, string(valid), true, true, http.StatusMethodNotAllowed},
		{"put", http.MethodPut, IONOSSharedPoolAgentPath, string(valid), true, true, http.StatusMethodNotAllowed},
		{"wrong-path", http.MethodPost, "/v1/ionos-shared-pool/other", string(valid), true, true, http.StatusNotFound},
		{"root-path", http.MethodPost, "/", string(valid), true, true, http.StatusNotFound},
		{"plaintext", http.MethodPost, IONOSSharedPoolAgentPath, string(valid), false, false, http.StatusForbidden},
		{"tls-without-client-cert", http.MethodPost, IONOSSharedPoolAgentPath, string(valid), true, false, http.StatusForbidden},
		{"malformed", http.MethodPost, IONOSSharedPoolAgentPath, `{"action":`, true, true, http.StatusBadRequest},
		{"empty", http.MethodPost, IONOSSharedPoolAgentPath, ``, true, true, http.StatusBadRequest},
		{"unknown-field", http.MethodPost, IONOSSharedPoolAgentPath, `{"action":"observe","spec":{},"extra":"x"}`, true, true, http.StatusBadRequest},
		{"trailing-token", http.MethodPost, IONOSSharedPoolAgentPath, string(valid) + ` {}`, true, true, http.StatusBadRequest},
		{"oversize", http.MethodPost, IONOSSharedPoolAgentPath, `{"action":"observe","pad":"` + strings.Repeat("x", (1<<20)+1) + `"}`, true, true, http.StatusRequestEntityTooLarge},
		{"empty-command", http.MethodPost, IONOSSharedPoolAgentPath, `{}`, true, true, http.StatusUnprocessableEntity},
	} {
		t.Run("http-"+tc.name, func(t *testing.T) {
			rec := serve(handler, tc.method, tc.path, tc.body, tc.secure, tc.verifiedClient)
			if rec.Code != tc.status {
				t.Fatalf("want %d, got %d %s", tc.status, rec.Code, rec.Body)
			}
			if tc.status == http.StatusMethodNotAllowed && rec.Header().Get("Allow") != http.MethodPost {
				t.Fatal("missing Allow header")
			}
			if (tc.name == "plaintext" || tc.name == "tls-without-client-cert") && !strings.Contains(rec.Body.String(), "mtls_required") {
				t.Fatalf("expected mtls_required refusal, got %s", rec.Body)
			}
			if len(r.calls) != before {
				t.Fatal("refused request reached the runtime")
			}
		})
	}

	// Runtime failures reach the caller as a fixed code, never as internal detail.
	r.failProvision = true
	provision, err := json.Marshal(IONOSSharedPoolCommand{Action: IONOSPoolProvision, Spec: mustIONOSSpec(t, node, ionosWork("tenant-a", "lost"))})
	if err != nil {
		t.Fatal(err)
	}
	rec := serve(handler, http.MethodPost, IONOSSharedPoolAgentPath, string(provision), true, true)
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "response lost") || !strings.Contains(rec.Body.String(), "runtime_failed") {
		t.Fatalf("runtime error leaked: %d %s", rec.Code, rec.Body)
	}
	r.failProvision = false

	// A runtime answering for another owner is never forwarded.
	r.wrongOwner = true
	rec = serve(handler, http.MethodPost, IONOSSharedPoolAgentPath, string(valid), true, true)
	r.wrongOwner = false
	if rec.Code != http.StatusBadGateway || strings.Contains(rec.Body.String(), "other-tenant") {
		t.Fatalf("foreign observation forwarded: %d %s", rec.Code, rec.Body)
	}

	// Misconfiguration fails closed.
	bad := ionosNode()
	bad.Location = "us/las"
	for name, misconfigured := range map[string]http.Handler{
		"node":    NewIONOSSharedPoolAgentHandler(bad, r),
		"runtime": NewIONOSSharedPoolAgentHandler(node, nil),
	} {
		if rec := serve(misconfigured, http.MethodPost, IONOSSharedPoolAgentPath, string(valid), true, true); rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s misconfiguration served: %d", name, rec.Code)
		}
	}
}

func mustIONOSSpec(t *testing.T, node IONOSSharedPoolNode, w IONOSSharedPoolWorkload) IONOSSharedPoolSpec {
	t.Helper()
	spec, err := ionosPoolSpec(node, w)
	if err != nil {
		t.Fatal(err)
	}
	return spec
}
