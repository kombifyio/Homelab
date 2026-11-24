package providercontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	CloudRunAdapterID = "cloud-run-v1"
	CloudRunRegion    = "europe-west3"
	CloudRunIsolation = "microvm"
)

var (
	ErrCloudRunSpec           = errors.New("providercontrol: invalid Cloud Run service specification")
	ErrCloudRunMemory         = errors.New("providercontrol: Cloud Run Gen2 requires 512 MiB for this 1 vCPU profile")
	ErrCloudRunIsolation      = errors.New("providercontrol: Cloud Run requires the Gen2 microVM sandbox without shared Docker or a Docker socket")
	ErrCloudRunOwnership      = errors.New("providercontrol: Cloud Run service does not match workload custody")
	ErrCloudRunPolicyDrift    = errors.New("providercontrol: Cloud Run service configuration failed policy readback")
	ErrCloudRunNotFound       = errors.New("providercontrol: Cloud Run service not found")
	ErrCloudRunAbsencePending = errors.New("providercontrol: Cloud Run service absence not yet confirmed")
	ErrCloudRunBackup         = errors.New("providercontrol: Cloud Run R2 backup or restore failed custody verification")
)

type CloudRunRegionError struct {
	Region string
}

func (*CloudRunRegionError) Error() string {
	return "providercontrol: Cloud Run requires explicit Frankfurt (europe-west3) placement"
}

// CloudRunOwner is bound into the immutable Cloud Run service labels. Every
// value must be a valid Cloud Run label value: 1 to 63 characters of lowercase
// letters, digits, underscores and dashes.
type CloudRunOwner struct {
	TenantID    string
	WorkloadID  string
	SlotRef     string
	CustodyHash string
}

type CloudRunHandle struct {
	Project     string
	Location    string
	ServiceName string
	Owner       CloudRunOwner
}

// CloudRunSpec is the whole requested placement. The adapter has no field for
// a VPC connector, Cloud SQL, a volume or a GCP-bound secret, so a caller that
// sets one of the Reject fields is refused before any provider traffic.
type CloudRunSpec struct {
	Region         string
	IsolationClass string
	MemoryMB       int
	Image          string
	InternalPort   int
	// Ingress is INGRESS_TRAFFIC_INTERNAL_ONLY when empty. The only other
	// admitted value is INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER.
	Ingress      string
	DockerSocket bool
	SharedDocker bool

	RejectedVPCConnector string
	RejectedCloudSQL     []string
	RejectedVolumes      []string
	RejectedBindings     []string
}

type CloudRunHealth string

const (
	CloudRunHealthUnknown   CloudRunHealth = "unknown"
	CloudRunHealthHealthy   CloudRunHealth = "healthy"
	CloudRunHealthUnhealthy CloudRunHealth = "unhealthy"
)

type CloudRunObservation struct {
	State  string
	Health CloudRunHealth
}

type CloudRunAbsence struct {
	Handle     CloudRunHandle
	ObservedAt time.Time
}

type CloudRunBackupArtifact struct {
	Ref    string
	SHA256 string
}

type CloudRunBackupManifest struct {
	Source   CloudRunHandle
	Artifact CloudRunBackupArtifact
}

// CloudRunR2Store resolves each opaque artifact reference inside the given
// workload's EU R2 custody, and verifies its digest before applying a restore.
// Backups never touch Google Cloud Storage or any other Google egress.
type CloudRunR2Store interface {
	Backup(context.Context, CloudRunHandle) (CloudRunBackupArtifact, error)
	Restore(context.Context, CloudRunHandle, CloudRunBackupArtifact) error
}

// CloudRunAdapter operates one dedicated Cloud Run service in europe-west3
// with a short-lived OAuth access token scoped to its project. Workload
// admission must supply a curated image and handles held by the workload
// ledger, never handles supplied by a user. Only this control plane may mutate
// the service or its ownership labels.
// It is not a server-lease executor and does not write or release custody.
// It is not registered with the provider registry.
type CloudRunAdapter struct {
	client  cloudRunClient
	project string
	service string
	owner   CloudRunOwner
	r2      CloudRunR2Store
}

// String, GoString and Format keep the access token out of any formatted view.
func (*CloudRunAdapter) String() string { return "providercontrol.CloudRunAdapter{redacted}" }

func (*CloudRunAdapter) GoString() string { return "providercontrol.CloudRunAdapter{redacted}" }

func (a *CloudRunAdapter) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, a.String())
}

func NewCloudRunAdapter(project, serviceName, token string, owner CloudRunOwner, r2 CloudRunR2Store) (*CloudRunAdapter, error) {
	return newCloudRunAdapter(project, serviceName, token, owner, r2, cloudRunAPIBase)
}

func newCloudRunAdapter(project, serviceName, token string, owner CloudRunOwner, r2 CloudRunR2Store, apiBase string) (*CloudRunAdapter, error) {
	if !cloudRunProjectID(project) || !cloudRunServiceName(serviceName) || token == "" || strings.ContainsAny(token, " \t\r\n") {
		return nil, ErrCloudRunSpec
	}
	for _, value := range owner.labels() {
		if !cloudRunLabelValue(value) {
			return nil, ErrCloudRunOwnership
		}
	}
	return &CloudRunAdapter{client: newCloudRunClient(apiBase, token), project: project, service: serviceName, owner: owner, r2: r2}, nil
}

// Provision issues one create, with no retry or adoption. Dispatch must be
// durably authorized at most once by the caller. The service name is fixed by
// the adapter, so once the create request has been sent the caller can always
// rebuild the handle for reconciliation. A returned handle must be retained
// even if configuration readback fails.
func (a *CloudRunAdapter) Provision(ctx context.Context, spec CloudRunSpec) (CloudRunHandle, error) {
	if spec.Region != CloudRunRegion {
		return CloudRunHandle{}, &CloudRunRegionError{Region: spec.Region}
	}
	if spec.IsolationClass != CloudRunIsolation || spec.DockerSocket || spec.SharedDocker {
		return CloudRunHandle{}, ErrCloudRunIsolation
	}
	if !cloudRunMemoryAllowed(spec.MemoryMB) {
		return CloudRunHandle{}, ErrCloudRunMemory
	}
	if spec.RejectedVPCConnector != "" || len(spec.RejectedCloudSQL) != 0 ||
		len(spec.RejectedVolumes) != 0 || len(spec.RejectedBindings) != 0 {
		return CloudRunHandle{}, ErrCloudRunSpec
	}
	ingress := spec.Ingress
	if ingress == "" {
		ingress = cloudRunIngressInternalOnly
	}
	if !cloudRunIngressAllowed(ingress) || !cloudRunImageValid(spec.Image) ||
		spec.InternalPort < 1 || spec.InternalPort > 65535 {
		return CloudRunHandle{}, ErrCloudRunSpec
	}
	request := cloudRunService{
		Labels:  a.owner.labels(),
		Ingress: ingress,
		Template: cloudRunTemplate{
			Scaling:              cloudRunRevisionScaling{MinInstanceCount: 0, MaxInstanceCount: 1},
			ExecutionEnvironment: cloudRunExecutionGen2,
			Containers: []cloudRunContainer{{
				Image: spec.Image,
				Ports: []cloudRunPort{{ContainerPort: spec.InternalPort}},
				Resources: cloudRunResources{
					Limits:  map[string]string{"cpu": "1000m", "memory": cloudRunMemoryLimit(spec.MemoryMB)},
					CPUIdle: true,
				},
			}},
		},
	}
	createPath := a.servicesPath(CloudRunRegion) + "?serviceId=" + url.QueryEscape(a.service)
	if err := a.client.request(ctx, http.MethodPost, createPath, request, nil); err != nil {
		return CloudRunHandle{}, err
	}
	handle := CloudRunHandle{Project: a.project, Location: CloudRunRegion, ServiceName: a.service, Owner: a.owner}
	service, err := a.readOwnedInRegion(ctx, handle)
	if err != nil {
		if errors.Is(err, ErrCloudRunNotFound) {
			return handle, ErrCloudRunResponse
		}
		return handle, err
	}
	container := service.Template.Containers
	if !service.satisfiesPolicy() || service.Ingress != ingress || container[0].Image != spec.Image ||
		container[0].Resources.Limits["memory"] != cloudRunMemoryLimit(spec.MemoryMB) ||
		container[0].Ports[0].ContainerPort != spec.InternalPort {
		return handle, ErrCloudRunPolicyDrift
	}
	return handle, nil
}

// Start verifies the exact owned service in europe-west3 against the policy.
// Cloud Run has no instance power switch: with a minimum of zero instances the
// first request starts one, so there is nothing to mutate and no way to wake it
// from the control plane. A drifted service, for example one that pins warm
// instances, is refused.
func (a *CloudRunAdapter) Start(ctx context.Context, handle CloudRunHandle) error {
	service, err := a.readOwnedInRegion(ctx, handle)
	if err != nil {
		return err
	}
	if !service.satisfiesPolicy() {
		return ErrCloudRunPolicyDrift
	}
	return nil
}

// Stop confirms ownership and that nothing keeps an instance warm. Cloud Run
// scales an idle service to zero by itself and offers no stop call. It reads at
// handle.Location so an owned service placed outside europe-west3 can still be
// confirmed stopped before Decommission.
func (a *CloudRunAdapter) Stop(ctx context.Context, handle CloudRunHandle) error {
	service, err := a.readOwned(ctx, handle)
	if err != nil {
		return err
	}
	if service.Template.Scaling.MinInstanceCount != 0 ||
		(service.Scaling != nil && (service.Scaling.MinInstanceCount != 0 || service.Scaling.ManualInstanceCount != nil)) {
		return ErrCloudRunPolicyDrift
	}
	return nil
}

func (a *CloudRunAdapter) Wake(ctx context.Context, handle CloudRunHandle) error {
	return a.Start(ctx, handle)
}

func (a *CloudRunAdapter) Observe(ctx context.Context, handle CloudRunHandle) (CloudRunObservation, error) {
	service, err := a.readOwnedInRegion(ctx, handle)
	if err != nil {
		return CloudRunObservation{}, err
	}
	if service.Template.ExecutionEnvironment != cloudRunExecutionGen2 {
		return CloudRunObservation{}, ErrCloudRunIsolation
	}
	return CloudRunObservation{State: service.state(), Health: service.health()}, nil
}

func (a *CloudRunAdapter) Backup(ctx context.Context, handle CloudRunHandle) (CloudRunBackupManifest, error) {
	if _, err := a.readOwnedInRegion(ctx, handle); err != nil {
		return CloudRunBackupManifest{}, err
	}
	if a.r2 == nil {
		return CloudRunBackupManifest{}, ErrCloudRunBackup
	}
	artifact, err := a.r2.Backup(ctx, handle)
	if err != nil || !artifact.valid() {
		return CloudRunBackupManifest{}, ErrCloudRunBackup
	}
	return CloudRunBackupManifest{Source: handle, Artifact: artifact}, nil
}

func (a *CloudRunAdapter) Restore(ctx context.Context, handle CloudRunHandle, manifest CloudRunBackupManifest) error {
	if manifest.Source != handle {
		return ErrCloudRunOwnership
	}
	service, err := a.readOwnedInRegion(ctx, handle)
	if err != nil {
		return err
	}
	if !service.satisfiesPolicy() {
		return ErrCloudRunPolicyDrift
	}
	if a.r2 == nil || !manifest.Artifact.valid() || a.r2.Restore(ctx, handle, manifest.Artifact) != nil {
		return ErrCloudRunBackup
	}
	return nil
}

// Decommission returns a provider observation for the exact recorded handle,
// not a custody release. An authorized retry may observe that the previous
// deletion already completed. A successful DELETE alone is never proof: only a
// separate GET that answers 404 is.
func (a *CloudRunAdapter) Decommission(ctx context.Context, handle CloudRunHandle) (CloudRunAbsence, error) {
	_, err := a.readOwned(ctx, handle)
	if errors.Is(err, ErrCloudRunNotFound) {
		return CloudRunAbsence{Handle: handle, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return CloudRunAbsence{}, err
	}
	err = a.client.request(ctx, http.MethodDelete, a.servicePath(handle.Location), nil, nil)
	if err != nil && !errors.Is(err, ErrCloudRunNotFound) {
		return CloudRunAbsence{}, err
	}
	_, err = a.readOwned(ctx, handle)
	if errors.Is(err, ErrCloudRunNotFound) {
		return CloudRunAbsence{Handle: handle, ObservedAt: time.Now().UTC()}, nil
	}
	if err != nil {
		return CloudRunAbsence{}, err
	}
	return CloudRunAbsence{}, ErrCloudRunAbsencePending
}

func (a *CloudRunAdapter) servicesPath(region string) string {
	return "/projects/" + a.project + "/locations/" + region + "/services"
}

func (a *CloudRunAdapter) servicePath(region string) string {
	return a.servicesPath(region) + "/" + a.service
}

func (a *CloudRunAdapter) ownsHandle(handle CloudRunHandle) bool {
	return handle.Project == a.project && handle.ServiceName == a.service && handle.Owner == a.owner &&
		cloudRunProjectID(handle.Project) && cloudRunServiceName(handle.ServiceName) &&
		cloudRunLocationID(handle.Location)
}

// readOwned reads the exact service the adapter owns at handle.Location so
// Stop and Decommission can clean up an owned service placed outside
// europe-west3. Active operations call readOwnedInRegion instead.
func (a *CloudRunAdapter) readOwned(ctx context.Context, handle CloudRunHandle) (cloudRunService, error) {
	if !a.ownsHandle(handle) {
		return cloudRunService{}, ErrCloudRunOwnership
	}
	var service cloudRunService
	if err := a.client.request(ctx, http.MethodGet, a.servicePath(handle.Location), nil, &service); err != nil {
		return cloudRunService{}, err
	}
	_, serviceID, ok := service.location()
	if !ok {
		return cloudRunService{}, ErrCloudRunResponse
	}
	if serviceID != handle.ServiceName {
		return cloudRunService{}, ErrCloudRunOwnership
	}
	for key, value := range a.owner.labels() {
		if service.Labels[key] != value {
			return cloudRunService{}, ErrCloudRunOwnership
		}
	}
	return service, nil
}

func (a *CloudRunAdapter) readOwnedInRegion(ctx context.Context, handle CloudRunHandle) (cloudRunService, error) {
	if !a.ownsHandle(handle) {
		return cloudRunService{}, ErrCloudRunOwnership
	}
	if handle.Location != CloudRunRegion {
		return cloudRunService{}, &CloudRunRegionError{Region: handle.Location}
	}
	service, err := a.readOwned(ctx, handle)
	if err != nil {
		return cloudRunService{}, err
	}
	if location, _, _ := service.location(); location != CloudRunRegion {
		return cloudRunService{}, &CloudRunRegionError{Region: location}
	}
	return service, nil
}

func (owner CloudRunOwner) labels() map[string]string {
	return map[string]string{
		"tenant_id": owner.TenantID, "workload_id": owner.WorkloadID,
		"slot_ref": owner.SlotRef, "custody_hash": owner.CustodyHash,
	}
}

func cloudRunMemoryAllowed(memoryMB int) bool {
	return memoryMB == 512
}

// cloudRunImageValid accepts a single-token OCI image reference: no
// whitespace, control characters, URL syntax or leading dash.
func cloudRunImageValid(image string) bool {
	if image == "" || len(image) > 1024 || strings.HasPrefix(image, "-") ||
		strings.ContainsAny(image, " \t\r\n\\?#\x00") {
		return false
	}
	for i := 0; i < len(image); i++ {
		if image[i] < 0x20 || image[i] >= 0x7f {
			return false
		}
	}
	return true
}

func (artifact CloudRunBackupArtifact) valid() bool {
	if !cloudRunBackupRefValid(artifact.Ref) {
		return false
	}
	return len(artifact.SHA256) == 64 &&
		strings.Trim(artifact.SHA256, "0123456789abcdef") == ""
}

func cloudRunBackupRefValid(ref string) bool {
	if ref == "" || len(ref) > 512 || strings.TrimSpace(ref) != ref ||
		strings.HasPrefix(ref, "/") || strings.HasSuffix(ref, "/") ||
		strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.ContainsAny(ref, " \t\r\n\\\x00") {
		return false
	}
	for i := 0; i < len(ref); i++ {
		if ref[i] < 0x20 || ref[i] == 0x7f {
			return false
		}
	}
	return true
}
