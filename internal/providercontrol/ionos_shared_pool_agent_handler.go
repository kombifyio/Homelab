package providercontrol

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const ionosSharedPoolAgentMaxBody = 1 << 20

// NewIONOSSharedPoolAgentTLSConfig builds the worker agent's server TLS
// configuration: TLS 1.3 minimum with mandatory client certificate verification
// against clientCAPEM.
func NewIONOSSharedPoolAgentTLSConfig(serverCert tls.Certificate, clientCAPEM []byte) (*tls.Config, error) {
	if len(serverCert.Certificate) == 0 || serverCert.PrivateKey == nil || len(clientCAPEM) == 0 {
		return nil, ionosPoolRefuse("agent_tls_refused", "supply a worker server certificate and client CA bundle for mTLS")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCAPEM) {
		return nil, ionosPoolRefuse("agent_tls_refused", "supply a valid PEM client CA bundle for mTLS")
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}, nil
}

// NewIONOSSharedPoolAgentHandler is the server side of IONOSSharedPoolHTTPRuntime:
// the node agent's single HTTPS route. It must be served with
// NewIONOSSharedPoolAgentTLSConfig (RequireAndVerifyClientCert); plaintext
// requests and TLS peers without a verified client certificate are refused with
// mtls_required. The handler never trusts a caller-supplied derived field; it
// recomputes the isolation spec from the workload and refuses a command whose
// spec differs, then dispatches to the node's runtime and verifies the
// observation before answering. An invalid node or a nil runtime yields a
// handler that refuses every request.
func NewIONOSSharedPoolAgentHandler(node IONOSSharedPoolNode, runtime IONOSSharedPoolRuntime) http.Handler {
	if node.validate() != nil || runtime == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			ionosPoolAgentError(w, http.StatusServiceUnavailable, "agent_misconfigured")
		})
	}
	return &ionosSharedPoolAgentHandler{node: node, runtime: runtime}
}

type ionosSharedPoolAgentHandler struct {
	node    IONOSSharedPoolNode
	runtime IONOSSharedPoolRuntime
}

func (h *ionosSharedPoolAgentHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != IONOSSharedPoolAgentPath {
		ionosPoolAgentError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		ionosPoolAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		ionosPoolAgentError(w, http.StatusForbidden, "mtls_required")
		return
	}
	body := http.MaxBytesReader(w, r.Body, ionosSharedPoolAgentMaxBody)
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	var command IONOSSharedPoolCommand
	if err := decoder.Decode(&command); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			ionosPoolAgentError(w, http.StatusRequestEntityTooLarge, "request_too_large")
			return
		}
		ionosPoolAgentError(w, http.StatusBadRequest, "request_malformed")
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		ionosPoolAgentError(w, http.StatusBadRequest, "request_malformed")
		return
	}
	expected, err := h.admit(command)
	if err != nil {
		ionosPoolAgentRefusal(w, http.StatusUnprocessableEntity, err)
		return
	}
	if command.Action == IONOSPoolVerifyBackup {
		verifier, ok := h.runtime.(IONOSSharedPoolBackupVerifier)
		if !ok || verifier.VerifyIONOSSharedPoolBackup(r.Context(), *command.Backup) != nil {
			ionosPoolAgentError(w, http.StatusBadGateway, "backup_refused")
			return
		}
		command.Action, command.Backup = IONOSPoolObserve, nil
	}
	observation, err := h.runtime.Execute(r.Context(), command)
	if err != nil {
		ionosPoolAgentRefusal(w, http.StatusBadGateway, err)
		return
	}
	if err := ionosPoolReadback(expected, observation); err != nil {
		ionosPoolAgentError(w, http.StatusBadGateway, "readback_refused")
		return
	}
	if observation.Backup != nil {
		if err := ionosPoolBackupShape(expected, observation.Backup); err != nil {
			ionosPoolAgentError(w, http.StatusBadGateway, "backup_refused")
			return
		}
	}
	var out bytes.Buffer
	if err := json.NewEncoder(&out).Encode(observation); err != nil {
		ionosPoolAgentError(w, http.StatusBadGateway, "response_refused")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
}

// admit re-validates the command against this worker and returns the spec the
// agent derives itself.
func (h *ionosSharedPoolAgentHandler) admit(command IONOSSharedPoolCommand) (IONOSSharedPoolSpec, error) {
	switch command.Action {
	case IONOSPoolVerifyBackup, IONOSPoolProvision, IONOSPoolStart, IONOSPoolStop, IONOSPoolObserve, IONOSPoolBackup, IONOSPoolRestore, IONOSPoolDecommission:
	default:
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("action_refused", "use a known lifecycle verb")
	}
	expected, err := ionosPoolSpec(h.node, command.Spec.Workload)
	if err != nil {
		return IONOSSharedPoolSpec{}, err
	}
	if command.Spec != expected {
		return IONOSSharedPoolSpec{}, ionosPoolRefuse("spec_refused", "send the spec the worker derives from the workload")
	}
	if command.Action != IONOSPoolRestore && command.Action != IONOSPoolVerifyBackup {
		if command.Backup != nil {
			return IONOSSharedPoolSpec{}, ionosPoolRefuse("backup_refused", "attach a backup manifest only to restore")
		}
		return expected, nil
	}
	if err := ionosPoolBackupShape(expected, command.Backup); err != nil {
		return IONOSSharedPoolSpec{}, err
	}
	return expected, nil
}

// ionosPoolBackupShape is the structural check shared by the adapter and the
// agent for an R2 manifest of one exact owning generation. It does not replace
// the authenticated R2 verification of the object.
func ionosPoolBackupShape(spec IONOSSharedPoolSpec, backup *IONOSSharedPoolBackupManifest) error {
	if backup == nil || backup.Spec != spec || backup.Store != "r2" || backup.Bucket == "" || backup.Version == "" || backup.SizeBytes <= 0 || !strings.HasPrefix(backup.Key, spec.VolumePrefix+"backups/") || strings.Contains(backup.Key, "..") || strings.ContainsAny(backup.Key, "\\\x00") {
		return ionosPoolRefuse("backup_refused", "use a verified R2 manifest for this exact owning tenant generation")
	}
	digest, err := hex.DecodeString(backup.SHA256)
	if err != nil || len(digest) != sha256.Size {
		return ionosPoolRefuse("backup_refused", "supply the R2 object's SHA256 digest")
	}
	return nil
}

// ionosPoolAgentRefusal answers with the refusal code only for policy denials;
// any other runtime error is reduced to a fixed code so no internals, paths or
// credentials leave the worker.
func ionosPoolAgentRefusal(w http.ResponseWriter, status int, err error) {
	var refusal *IONOSSharedPoolRefusal
	if errors.As(err, &refusal) {
		ionosPoolAgentError(w, status, refusal.Code)
		return
	}
	ionosPoolAgentError(w, status, "runtime_failed")
}

func ionosPoolAgentError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: code})
}
