package providercontrol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const IONOSSharedPoolMaxCommandDuration = IONOSPoolSynchronousBudget

// IONOSSharedPoolHTTPRuntime speaks the dedicated node-agent protocol, not the
// IONOS Cloud API. The supplied client's transport must authenticate the admitted
// worker (e.g. pinned mutual TLS with node-held credentials). No raw credentials
// enter commands or custody. A node agent implementing the runtime contract is
// required before production registration; this client does not install one.
type IONOSSharedPoolHTTPRuntime struct {
	endpoint string
	client   http.Client
}

func NewIONOSSharedPoolHTTPRuntime(origin string, client *http.Client) (*IONOSSharedPoolHTTPRuntime, error) {
	endpoint, err := url.Parse(origin)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") || client == nil {
		return nil, ionosPoolRefuse("agent_endpoint_refused", "configure an authenticated HTTPS worker origin and client")
	}
	copy := *client
	// Never forward node credentials or mutations through a redirect.
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if copy.Timeout <= 0 || copy.Timeout > IONOSSharedPoolMaxCommandDuration {
		copy.Timeout = IONOSSharedPoolMaxCommandDuration
	}
	endpoint.Path = IONOSSharedPoolAgentPath
	return &IONOSSharedPoolHTTPRuntime{endpoint: endpoint.String(), client: copy}, nil
}

func (r *IONOSSharedPoolHTTPRuntime) Execute(ctx context.Context, command IONOSSharedPoolCommand) (IONOSSharedPoolObservation, error) {
	if command.Action != IONOSPoolBackup && command.Action != IONOSPoolRestore && command.Action != IONOSPoolVerifyBackup {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
	}
	payload, err := json.Marshal(command)
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(payload))
	if err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := r.client.Do(request)
	if err != nil {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("agent_transport_failed", "reconcile the exact generation before retrying; retain its capacity")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return IONOSSharedPoolObservation{}, fmt.Errorf("IONOS worker agent returned HTTP %d; retain custody and reconcile", response.StatusCode)
	}
	const maxResponse = 1 << 20
	body, err := io.ReadAll(io.LimitReader(response.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("agent_response_refused", "require a bounded complete agent response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var observation IONOSSharedPoolObservation
	if err := decoder.Decode(&observation); err != nil {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("agent_response_refused", "require the exact worker response schema")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return IONOSSharedPoolObservation{}, ionosPoolRefuse("agent_response_refused", "require a single worker response")
	}
	if err := ionosPoolReadback(command.Spec, observation); err != nil {
		return IONOSSharedPoolObservation{}, err
	}
	return observation, nil
}

var _ IONOSSharedPoolRuntime = (*IONOSSharedPoolHTTPRuntime)(nil)

const IONOSPoolVerifyBackup IONOSSharedPoolAction = "verify_backup"

func (r *IONOSSharedPoolHTTPRuntime) VerifyIONOSSharedPoolBackup(ctx context.Context, manifest IONOSSharedPoolBackupManifest) error {
	_, err := r.Execute(ctx, IONOSSharedPoolCommand{Action: IONOSPoolVerifyBackup, Spec: manifest.Spec, Backup: &manifest})
	return err
}
