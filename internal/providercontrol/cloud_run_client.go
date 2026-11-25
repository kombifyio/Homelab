package providercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	cloudRunAPIBase             = "https://run.googleapis.com/v2"
	cloudRunResponseLimit       = 1 << 20
	cloudRunDefaultPollInterval = 2 * time.Second
	cloudRunOperationWaitLimit  = 2 * time.Minute

	cloudRunExecutionGen2       = "EXECUTION_ENVIRONMENT_GEN2"
	cloudRunIngressInternalOnly = "INGRESS_TRAFFIC_INTERNAL_ONLY"
	cloudRunIngressInternalLB   = "INGRESS_TRAFFIC_INTERNAL_LOAD_BALANCER"
	cloudRunConditionSucceeded  = "CONDITION_SUCCEEDED"
	cloudRunConditionFailed     = "CONDITION_FAILED"
)

var (
	ErrCloudRunResponse = errors.New("providercontrol: invalid Cloud Run Admin API response")

	cloudRunProjectPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	cloudRunProjectNumPattern = regexp.MustCompile(`^[0-9]{1,32}$`)
	cloudRunServicePattern    = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,47}[a-z0-9])?$`)
	cloudRunLocationPattern   = regexp.MustCompile(`^[a-z][a-z0-9-]{1,61}[a-z0-9]$`)
	cloudRunOperationPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
	cloudRunLabelPattern      = regexp.MustCompile(`^[a-z0-9_-]{1,63}$`)
)

// CloudRunAPIError reports a non-success HTTP status from the Cloud Run Admin
// API. It never carries a response body, a credential or a request header.
type CloudRunAPIError struct {
	StatusCode int
}

func (e *CloudRunAPIError) Error() string {
	return fmt.Sprintf("providercontrol: Cloud Run Admin API returned HTTP %d", e.StatusCode)
}

type cloudRunClient struct {
	base  string
	token string
	http  *http.Client
}

func newCloudRunClient(base, token string) cloudRunClient {
	return cloudRunClient{
		base: base, token: token,
		http: &http.Client{
			Timeout: 30 * time.Second,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// String, GoString and Format keep the bearer token out of every formatted
// view of the client, including values nested in a larger struct.
func (cloudRunClient) String() string { return "providercontrol.cloudRunClient{redacted}" }

func (cloudRunClient) GoString() string { return "providercontrol.cloudRunClient{redacted}" }

func (c cloudRunClient) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, c.String())
}

func (c cloudRunClient) request(ctx context.Context, method, path string, payload, result any) error {
	var body io.Reader
	if payload != nil {
		data, err := json.Marshal(payload)
		if err != nil {
			return ErrCloudRunSpec
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return ErrCloudRunSpec
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// The transport error text can echo the request URL; keep it out.
		return fmt.Errorf("%w: transport failure", ErrCloudRunResponse)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound && (method == http.MethodGet || method == http.MethodDelete) {
		return ErrCloudRunNotFound
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return &CloudRunAPIError{StatusCode: response.StatusCode}
	}
	if result == nil {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return ErrCloudRunResponse
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, cloudRunResponseLimit+1))
	if err != nil || len(data) > cloudRunResponseLimit || json.Unmarshal(data, result) != nil {
		return ErrCloudRunResponse
	}
	return nil
}

func cloudRunProjectID(value string) bool {
	return cloudRunProjectPattern.MatchString(value)
}

func cloudRunServiceName(value string) bool {
	return cloudRunServicePattern.MatchString(value)
}

func cloudRunLocationID(value string) bool {
	return cloudRunLocationPattern.MatchString(value)
}

func cloudRunLabelValue(value string) bool {
	return cloudRunLabelPattern.MatchString(value)
}

// cloudRunService is the subset of the Cloud Run Admin API v2 Service resource
// this adapter writes and reads back. JSON names are the REST camelCase names.
type cloudRunService struct {
	Name                string                  `json:"name,omitempty"`
	Labels              map[string]string       `json:"labels"`
	Ingress             string                  `json:"ingress"`
	InvokerIAMDisabled  bool                    `json:"invokerIamDisabled,omitempty"`
	MultiRegionSettings json.RawMessage         `json:"multiRegionSettings,omitempty"`
	Scaling             *cloudRunServiceScaling `json:"scaling,omitempty"`
	Template            cloudRunTemplate        `json:"template"`
	Reconciling         bool                    `json:"reconciling,omitempty"`
	TerminalCondition   *cloudRunCondition      `json:"terminalCondition,omitempty"`
}

type cloudRunServiceScaling struct {
	MinInstanceCount    int    `json:"minInstanceCount,omitempty"`
	ScalingMode         string `json:"scalingMode,omitempty"`
	ManualInstanceCount *int   `json:"manualInstanceCount,omitempty"`
}

type cloudRunTemplate struct {
	Scaling              cloudRunRevisionScaling `json:"scaling"`
	VPCAccess            json.RawMessage         `json:"vpcAccess,omitempty"`
	ExecutionEnvironment string                  `json:"executionEnvironment"`
	Containers           []cloudRunContainer     `json:"containers"`
	Volumes              []json.RawMessage       `json:"volumes,omitempty"`
}

type cloudRunRevisionScaling struct {
	MinInstanceCount int `json:"minInstanceCount"`
	MaxInstanceCount int `json:"maxInstanceCount"`
}

type cloudRunContainer struct {
	Image        string            `json:"image"`
	Env          []cloudRunEnvVar  `json:"env,omitempty"`
	Ports        []cloudRunPort    `json:"ports"`
	Resources    cloudRunResources `json:"resources"`
	VolumeMounts []json.RawMessage `json:"volumeMounts,omitempty"`
}

type cloudRunEnvVar struct {
	Name        string          `json:"name"`
	Value       string          `json:"value,omitempty"`
	ValueSource json.RawMessage `json:"valueSource,omitempty"`
}

type cloudRunPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int    `json:"containerPort"`
}

type cloudRunResources struct {
	Limits  map[string]string `json:"limits"`
	CPUIdle bool              `json:"cpuIdle"`
}

type cloudRunCondition struct {
	Type  string `json:"type"`
	State string `json:"state"`
}

type cloudRunOperation struct {
	Name  string           `json:"name,omitempty"`
	Done  bool             `json:"done,omitempty"`
	Error *cloudRunOpError `json:"error,omitempty"`
}

type cloudRunOpError struct {
	Code int `json:"code,omitempty"`
}

// location validates and extracts the location and operation ID from
// "projects/{project}/locations/{location}/operations/{opID}" without letting
// the adapter follow arbitrary paths returned by a provider response.
func (op cloudRunOperation) location(expectedProject string) (location, opID string, ok bool) {
	parts := strings.Split(op.Name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "operations" {
		return "", "", false
	}
	if parts[1] != expectedProject && !cloudRunProjectNumPattern.MatchString(parts[1]) {
		return "", "", false
	}
	if !cloudRunLocationID(parts[3]) || !cloudRunOperationPattern.MatchString(parts[5]) {
		return "", "", false
	}
	return parts[3], parts[5], true
}

// cloudRunRawAbsent reports whether an optional JSON object is missing, null or
// empty, which the API may emit for an unset message field.
func cloudRunRawAbsent(raw json.RawMessage) bool {
	switch strings.TrimSpace(string(raw)) {
	case "", "null", "{}":
		return true
	}
	return false
}

func cloudRunIngressAllowed(ingress string) bool {
	return ingress == cloudRunIngressInternalOnly || ingress == cloudRunIngressInternalLB
}

func cloudRunMemoryLimit(memoryMB int) string {
	return fmt.Sprintf("%dMi", memoryMB)
}

// location extracts the Cloud Run location and service ID from the resource
// name "projects/{project}/locations/{location}/services/{service}". The
// project segment may be an ID or a number, so only the path scope the request
// was made under identifies it.
func (service cloudRunService) location() (location, serviceID string, ok bool) {
	parts := strings.Split(service.Name, "/")
	if len(parts) != 6 || parts[0] != "projects" || parts[2] != "locations" || parts[4] != "services" ||
		parts[1] == "" || parts[3] == "" || parts[5] == "" {
		return "", "", false
	}
	return parts[3], parts[5], true
}

// satisfiesPolicy is the provider-side readback of the shared-substrate
// profile: one 1 vCPU container of 512 MiB on the Gen2 microVM sandbox,
// single-region scale to zero with at most one instance, IAM invoker enabled,
// no VPC connector, volume, Cloud SQL, Secret Manager or other GCP-bound
// attachment, and private ingress.
func (service cloudRunService) satisfiesPolicy() bool {
	template := service.Template
	if !cloudRunIngressAllowed(service.Ingress) ||
		service.InvokerIAMDisabled ||
		!cloudRunRawAbsent(service.MultiRegionSettings) ||
		template.ExecutionEnvironment != cloudRunExecutionGen2 ||
		template.Scaling.MinInstanceCount != 0 || template.Scaling.MaxInstanceCount != 1 ||
		!cloudRunRawAbsent(template.VPCAccess) || len(template.Volumes) != 0 || len(template.Containers) != 1 {
		return false
	}
	if scaling := service.Scaling; scaling != nil &&
		(scaling.MinInstanceCount != 0 || scaling.ManualInstanceCount != nil ||
			(scaling.ScalingMode != "" && scaling.ScalingMode != "AUTOMATIC")) {
		return false
	}
	container := template.Containers[0]
	if container.Image == "" || len(container.VolumeMounts) != 0 || len(container.Ports) != 1 ||
		container.Ports[0].ContainerPort < 1 || container.Ports[0].ContainerPort > 65535 {
		return false
	}
	for _, variable := range container.Env {
		if !cloudRunRawAbsent(variable.ValueSource) {
			return false
		}
	}
	limits := container.Resources.Limits
	return len(limits) == 2 && (limits["cpu"] == "1000m" || limits["cpu"] == "1") &&
		cloudRunMemoryLimitAllowed(limits["memory"]) && container.Resources.CPUIdle
}

func cloudRunMemoryLimitAllowed(limit string) bool {
	return limit == cloudRunMemoryLimit(512)
}

func (service cloudRunService) health() CloudRunHealth {
	condition := service.TerminalCondition
	if condition == nil || condition.Type != "Ready" {
		return CloudRunHealthUnknown
	}
	switch {
	case condition.State == cloudRunConditionFailed:
		return CloudRunHealthUnhealthy
	case condition.State == cloudRunConditionSucceeded && !service.Reconciling && service.satisfiesPolicy():
		return CloudRunHealthHealthy
	}
	return CloudRunHealthUnknown
}

func (service cloudRunService) state() string {
	condition := service.TerminalCondition
	switch {
	case condition != nil && condition.State == cloudRunConditionFailed:
		return "failed"
	case condition != nil && condition.Type == "Ready" && condition.State == cloudRunConditionSucceeded && !service.Reconciling:
		return "ready"
	}
	return "reconciling"
}
