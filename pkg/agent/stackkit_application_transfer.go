package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kombifyio/techstack/pkg/api/agentpb"
	"github.com/kombifyio/techstack/pkg/stackkitcommand"
)

// applicationTransferExportRoot is the node-owned parent of every export the
// Guard asks StackKits for; an export directory is named by its operation.
const applicationTransferExportRoot = ".stackkit/backups/transfer-exports"

// stackKitApplicationTransferArgs maps one closed transfer step to the fixed
// pinned-CLI argv. Documents and the capability become private 0600 files in
// one private directory beneath the workspace; no value becomes shell text.
func stackKitApplicationTransferArgs(workDir string, command *agentpb.StackKitCommand) ([]string, []string, error) {
	if err := stackkitcommand.ValidateApplicationTransferFields(command); err != nil {
		return nil, nil, err
	}
	request, err := stackkitcommand.DecodeApplicationTransferRequest(command.ApplicationTransferJson)
	if err != nil {
		return nil, nil, err
	}
	directory, err := os.MkdirTemp(workDir, ".techstack-transfer-")
	if err != nil {
		return nil, nil, err
	}
	files := []string{}
	fail := func(cause error) ([]string, []string, error) {
		removeStackKitTempFiles(append(files, directory))
		return nil, nil, cause
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return fail(err)
	}
	write := func(name string, data []byte) (string, error) {
		path := filepath.Join(directory, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", err
		}
		files = append(files, path)
		return path, nil
	}
	document := func(name string, data json.RawMessage) string {
		path, writeErr := write(name, data)
		if writeErr != nil && err == nil {
			err = writeErr
		}
		return path
	}
	capability := ""
	if len(command.AdvancedCapability) != 0 {
		capability = document("capability.json", command.AdvancedCapability)
	}
	exportDir := ""
	if request.OperationID != "" {
		exportDir = filepath.Join(workDir, filepath.FromSlash(applicationTransferExportRoot), request.OperationID)
	}
	selection := func(args []string) []string {
		if request.UseCase != "" {
			return append(args, "--use-case", request.UseCase)
		}
		for _, workload := range request.Workloads {
			args = append(args, "--workload", workload)
		}
		return args
	}
	sourceAdmission := func(args []string) []string {
		return append(args, "--archive-sha256", request.ArchiveSHA256, "--source-receipt", document("source-receipt.json", request.SourceReceipt),
			"--source-owner", request.SourceOwner, "--source-fingerprint", request.SourceFingerprint, "--transfer-id", request.TransferID,
			"--expected-plan-hash", request.ExpectedPlanHash)
	}
	var args []string
	switch request.Step {
	case stackkitcommand.TransferStepConfiguration:
		args = selection([]string{"backup", "transfer-configuration"})
	case stackkitcommand.TransferStepCandidate:
		candidate := filepath.Join(directory, "candidate.json")
		files = append(files, candidate)
		args = []string{"backup", "transfer-candidate", "--portable-configuration", document("portable-configuration.json", request.PortableConfiguration), "--output", candidate}
	case stackkitcommand.TransferStepTarget:
		args = []string{"backup", "transfer-target", "--candidate-spec", document("candidate.json", request.CandidateSpec), "--expected-spec-hash", request.ExpectedSpecHash,
			"--source-owner", request.SourceOwner, "--portable-configuration-digest", request.PortableConfigurationDigest, "--capability", capability}
	case stackkitcommand.TransferStepExport:
		if mkdirErr := os.MkdirAll(filepath.Dir(exportDir), 0o700); mkdirErr != nil {
			return fail(mkdirErr)
		}
		args = selection([]string{"backup", "transfer-export", "--mode", "migrate"})
		args = append(args, "--destination", document("destination.json", request.DestinationAdmission), "--destination-owner", request.DestinationOwner,
			"--destination-fingerprint", request.DestinationFingerprint, "--target", exportDir, "--operation-id", request.OperationID,
			"--expected-plan-hash", request.ExpectedPlanHash)
	case stackkitcommand.TransferStepServe:
		peer, peerErr := stackkitcommand.ApplicationTransferPeer(request.Peer)
		if peerErr != nil {
			return fail(peerErr)
		}
		args = []string{"backup", "transfer-serve", "--target", exportDir, "--peer", peer, "--port", strconv.Itoa(stackkitcommand.ApplicationTransferPort), "--open-firewall", "--ttl", "10m"}
	case stackkitcommand.TransferStepFetch:
		peer, peerErr := stackkitcommand.ApplicationTransferPeer(request.Peer)
		if peerErr != nil {
			return fail(peerErr)
		}
		args = []string{"backup", "transfer-fetch", "--from", net.JoinHostPort(peer, strconv.Itoa(stackkitcommand.ApplicationTransferPort)), "--archive-sha256", request.ArchiveSHA256, "--wait", "5m"}
	case stackkitcommand.TransferStepPrepare:
		args = sourceAdmission([]string{"backup", "transfer-prepare"})
		args = append(args, "--capability", capability, "--owner-approve")
	case stackkitcommand.TransferStepImport:
		args = sourceAdmission([]string{"backup", "transfer-import"})
		args = append(args, "--capability", capability, "--owner-approve")
	case stackkitcommand.TransferStepRecover:
		args = sourceAdmission([]string{"backup", "transfer-recover"})
		args = append(args, "--capability", capability, "--owner-approve")
	case stackkitcommand.TransferStepAbort:
		args = sourceAdmission([]string{"backup", "transfer-abort"})
		if capability != "" {
			args = append(args, "--capability", capability)
		}
		args = append(args, "--owner-approve")
	case stackkitcommand.TransferStepFinish, stackkitcommand.TransferStepResume:
		verb := "transfer-" + request.Step
		args = []string{"backup", verb, "--source-receipt", document("source-receipt.json", request.SourceReceipt),
			"--target-outcome", document("target-outcome.json", request.TargetOutcome), "--target-fingerprint", request.DestinationFingerprint}
		if capability != "" {
			args = append(args, "--capability", capability)
		}
		args = append(args, "--owner-approve")
	case stackkitcommand.TransferStepWithdraw:
		args = []string{"backup", "transfer-withdraw", "--nonce", request.Nonce, "--owner-approve"}
	default:
		return fail(fmt.Errorf("application transfer step %q is unsupported", request.Step))
	}
	if err != nil {
		return fail(err)
	}
	return args, append(files, directory), nil
}

// collectApplicationTransferDocument wraps the step's own JSON document in
// the one transfer envelope. The documents are public by contract (signed
// admissions, receipts, outcomes, transport reports, an authored candidate)
// and must reach the other node byte for byte, so they are read from the
// CLI's own output and never from the redacted copy. A failed step keeps a
// document when the CLI wrote one (a denial naming the scope it awaits).
func collectApplicationTransferDocument(command *agentpb.StackKitCommand, stdout []byte, tempFiles []string, runErr error) ([]byte, error) {
	request, err := stackkitcommand.DecodeApplicationTransferRequest(command.ApplicationTransferJson)
	if err != nil {
		return nil, err
	}
	document := lastJSONDocument(stdout)
	if len(document) != 0 {
		// A StackKits command-result envelope carries its document in data.
		var envelope struct {
			SchemaVersion string          `json:"schemaVersion"`
			Data          json.RawMessage `json:"data"`
		}
		if json.Unmarshal(document, &envelope) == nil && envelope.SchemaVersion == "stackkit.command-result/v1" && len(envelope.Data) != 0 {
			document = envelope.Data
		}
	}
	if runErr == nil && request.Step == stackkitcommand.TransferStepCandidate {
		document, err = attachApplicationTransferCandidate(document, tempFiles)
		if err != nil {
			return nil, err
		}
	}
	if runErr == nil && len(document) == 0 {
		return nil, fmt.Errorf("application transfer step %s returned no JSON document", request.Step)
	}
	status := "success"
	data := map[string]any{"step": request.Step}
	if len(document) != 0 {
		data["document"] = json.RawMessage(document)
	}
	if runErr != nil {
		status = "failed"
		data["error"] = runErr.Error()
	}
	return json.Marshal(map[string]any{
		"schemaVersion": "stackkit.command-result/v1",
		"command":       stackkitcommand.ResultCommandName(command.Operation),
		"status":        status,
		"data":          data,
	})
}

// lastJSONDocument returns the JSON object that ends stdout: the whole output,
// or the last object that starts on its own line after human text.
func lastJSONDocument(stdout []byte) []byte {
	trimmed := bytes.TrimSpace(stdout)
	if json.Valid(trimmed) {
		return trimmed
	}
	for index := len(trimmed) - 1; index > 0; index-- {
		if trimmed[index] == '{' && trimmed[index-1] == '\n' && json.Valid(trimmed[index:]) {
			return trimmed[index:]
		}
	}
	return nil
}

// attachApplicationTransferCandidate adds the authored candidate bytes to the
// stackkit.intent-change/v1 document the candidate step returns.
func attachApplicationTransferCandidate(document []byte, tempFiles []string) ([]byte, error) {
	candidatePath := ""
	for _, file := range tempFiles {
		if filepath.Base(file) == "candidate.json" && strings.HasPrefix(filepath.Base(filepath.Dir(file)), ".techstack-transfer-") {
			candidatePath = file
		}
	}
	var data map[string]json.RawMessage
	if candidatePath == "" || json.Unmarshal(document, &data) != nil {
		return nil, fmt.Errorf("receiving candidate custody is unavailable")
	}
	var path string
	if json.Unmarshal(data["path"], &path) != nil || path != candidatePath {
		return nil, fmt.Errorf("receiving candidate named another path")
	}
	file, err := os.Open(candidatePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > stackkitcommand.MaxInitCandidateBytes {
		return nil, fmt.Errorf("receiving candidate is not a bounded regular file")
	}
	candidate, err := io.ReadAll(io.LimitReader(file, stackkitcommand.MaxInitCandidateBytes+1))
	if err != nil || !json.Valid(candidate) {
		return nil, fmt.Errorf("receiving candidate bytes are invalid")
	}
	data["candidate_spec"] = json.RawMessage(candidate)
	delete(data, "path")
	return json.Marshal(data)
}
