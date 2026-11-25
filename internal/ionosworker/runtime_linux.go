package ionosworker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kombifyio/homelab/internal/providercontrol"
)

const (
	dataDirectory = "/var/lib/kombify/ionos-shared-worker"
	policyPath    = "/etc/kombify/ionos-shared-worker/workload-net.nft"
	runscPath     = "/usr/bin/runsc"
)

var (
	imageRef = regexp.MustCompile(`^([a-z0-9-]+(\.[a-z0-9-]+)+(:[0-9]+)?|[a-z0-9-]+:[0-9]+)/[a-z0-9][a-z0-9._/-]*@sha256:[0-9a-f]{64}$`)
	name     = regexp.MustCompile(`^net-[0-9a-f]{64}$`)
)

type Application struct {
	Image       string   `json:"image"`
	DataPath    string   `json:"data_path"`
	HealthCheck []string `json:"health_check"`
	UID         int      `json:"uid"`
	GID         int      `json:"gid"`
}

type generation struct {
	Spec    providercontrol.IONOSSharedPoolSpec `json:"spec"`
	App     Application                         `json:"app"`
	Slot    int                                 `json:"slot"`
	Removed bool                                `json:"removed"`
}

type Runtime struct {
	mu       sync.Mutex
	node     providercontrol.IONOSSharedPoolNode
	apps     map[string]Application
	backup   *BackupStore
	netRules []byte
}

func New(ctx context.Context, node providercontrol.IONOSSharedPoolNode, apps []Application, backup *BackupStore) (*Runtime, error) {
	if _, err := providercontrol.BuildIONOSSharedWorkerProfile(node); err != nil {
		return nil, err
	}
	if len(apps) == 0 || backup == nil {
		return nil, errors.New("worker requires an image allowlist and R2 custody")
	}
	approved := make(map[string]Application, len(apps))
	for _, app := range apps {
		if !canonicalImage(app.Image) || !strings.HasPrefix(app.DataPath, "/") || path.Clean(app.DataPath) != app.DataPath || app.DataPath == "/" || strings.ContainsAny(app.DataPath, ":,\n\r\x00") || len(app.HealthCheck) == 0 || app.HealthCheck[0] == "" || app.UID < 1 || app.UID > 65535 || app.GID < 1 || app.GID > 65535 {
			return nil, errors.New("invalid worker image, data path or health check")
		}
		if _, exists := approved[app.Image]; exists {
			return nil, errors.New("duplicate worker image")
		}
		approved[app.Image] = app
	}
	if err := checkStorage(ctx, node.Slots); err != nil {
		return nil, err
	}
	for _, directory := range []string{dataDirectory, filepath.Join(dataDirectory, "generations"), filepath.Join(dataDirectory, "volumes")} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
	}
	rules, err := os.ReadFile(policyPath)
	if err != nil || len(rules) == 0 {
		return nil, errors.New("worker requires the installed namespace firewall")
	}
	if _, err := command(ctx, runscPath, "--version"); err != nil {
		return nil, err
	}
	if _, err := podman(ctx, "info"); err != nil {
		return nil, err
	}
	if _, err := command(ctx, "/usr/sbin/nft", "list", "table", "inet", "kombify_shared_worker"); err != nil {
		return nil, err
	}
	return &Runtime{node: node, apps: approved, backup: backup, netRules: rules}, nil
}

func canonicalImage(image string) bool {
	if !imageRef.MatchString(image) {
		return false
	}
	registry, repository, _ := strings.Cut(image, "/")
	return registry != "index.docker.io" && (registry != "docker.io" || strings.Contains(repository, "/"))
}

func (runtime *Runtime) Execute(ctx context.Context, request providercontrol.IONOSSharedPoolCommand) (_ providercontrol.IONOSSharedPoolObservation, runtimeErr error) {
	if !runtime.mu.TryLock() {
		return providercontrol.IONOSSharedPoolObservation{}, &providercontrol.IONOSSharedPoolRefusal{Code: "worker_busy", Repair: "reconcile and retry after the current operation"}
	}
	defer runtime.mu.Unlock()
	defer func() {
		if runtimeErr != nil {
			log.Printf("worker action=%s: %v", request.Action, runtimeErr)
		}
	}()
	empty := providercontrol.IONOSSharedPoolObservation{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	spec := request.Spec
	if spec.NodeID != runtime.node.ID || !name.MatchString(spec.NetworkNamespace) || spec.RuntimeBinary != "runsc" {
		return empty, &providercontrol.IONOSSharedPoolRefusal{Code: "worker_policy_refused", Repair: "select the enrolled gVisor worker"}
	}
	if err := checkStorage(ctx, runtime.node.Slots); err != nil {
		return empty, err
	}
	id := spec.NetworkNamespace
	volume := filepath.Join(dataDirectory, "volumes", id, "data")
	record, err := readGeneration(id)
	if err != nil {
		return empty, err
	}
	if record != nil && record.Spec != spec {
		return empty, errors.New("worker generation custody mismatch")
	}
	app, approved := runtime.apps[spec.Workload.Image]
	if (request.Action == providercontrol.IONOSPoolProvision || request.Action == providercontrol.IONOSPoolStart) &&
		(!approved || (record != nil && !record.App.samePolicy(app))) {
		return empty, &providercontrol.IONOSSharedPoolRefusal{Code: "worker_image_refused", Repair: "admit the exact pinned application policy"}
	}
	if record == nil && request.Action == providercontrol.IONOSPoolProvision {
		if _, err := confirmAbsent(ctx, spec, volume); err != nil {
			return empty, err
		}
		record, err = runtime.reserve(spec, app)
		if err != nil {
			return empty, err
		}
	}
	if record == nil || record.Removed {
		if request.Action != providercontrol.IONOSPoolObserve && request.Action != providercontrol.IONOSPoolDecommission {
			return empty, errors.New("worker generation is not allocated")
		}
		return confirmAbsent(ctx, spec, volume)
	}
	app = record.App
	container, err := inspectContainer(ctx, id)
	if err != nil {
		return empty, err
	}
	if container != nil && !container.matches(id, app, volume, spec.Workload.MemoryMB) {
		return empty, errors.New("worker container does not match generation policy")
	}
	if container != nil && request.Action != providercontrol.IONOSPoolDecommission && request.Action != providercontrol.IONOSPoolStop {
		if err := checkVolumeQuota(ctx, record.Slot, volume); err != nil {
			return empty, err
		}
	}
	var backup *providercontrol.IONOSSharedPoolBackupManifest
	switch request.Action {
	case providercontrol.IONOSPoolProvision:
		if container == nil {
			if err := runtime.provision(ctx, record, app, volume); err != nil {
				return empty, err
			}
		} else if !container.State.Running {
			if err := runtime.ensureNetwork(ctx, record); err != nil {
				return empty, err
			}
		}
	case providercontrol.IONOSPoolStart:
		if container == nil {
			return empty, errors.New("provision the exact generation before starting it")
		}
		if !container.State.Running {
			if err := runtime.ensureNetwork(ctx, record); err != nil {
				return empty, err
			}
			if _, err := podman(ctx, "start", id); err != nil {
				return empty, err
			}
		}
		if err := waitHealthy(ctx, id); err != nil {
			return empty, err
		}
	case providercontrol.IONOSPoolStop:
		if container != nil && container.State.Running {
			if _, err := podman(ctx, "stop", "--time", "10", id); err != nil {
				return empty, err
			}
		}
	case providercontrol.IONOSPoolObserve:
		if container == nil {
			return stoppedObservation(spec, "allocation-without-container"), nil
		}
	case providercontrol.IONOSPoolBackup, providercontrol.IONOSPoolRestore:
		if container == nil || container.State.Running {
			return empty, errors.New("backup and restore require a stopped generation")
		}
		if request.Action == providercontrol.IONOSPoolBackup {
			backup, err = runtime.backup.save(ctx, spec, volume)
		} else {
			err = runtime.backup.restore(ctx, spec, volume, request.Backup)
		}
		if err != nil {
			return empty, err
		}
	case providercontrol.IONOSPoolDecommission:
		if container != nil {
			if _, err := podman(ctx, "rm", "--force", "--time", "10", id); err != nil {
				return empty, err
			}
		}
		if err := removeNetwork(ctx, record); err != nil {
			return empty, err
		}
		if err := os.RemoveAll(filepath.Dir(volume)); err != nil {
			return empty, err
		}
		observed, err := confirmAbsent(ctx, spec, volume)
		if err != nil {
			return empty, err
		}
		record.Removed = true
		return observed, writeGeneration(record)
	default:
		return empty, errors.New("unknown worker action")
	}
	container, err = inspectContainer(ctx, id)
	if err != nil || container == nil || !container.matches(id, app, volume, spec.Workload.MemoryMB) {
		return empty, errors.New("worker container readback failed")
	}
	if container.State.Running || request.Action == providercontrol.IONOSPoolProvision {
		if err := runtime.checkNetwork(ctx, record); err != nil {
			return empty, err
		}
	}
	state := "stopped"
	if container.State.Running {
		state = "running"
	}
	return providercontrol.IONOSSharedPoolObservation{Spec: spec, State: state, EvidenceRef: "ionos-worker:" + runtime.node.ID + ":" + id + ":" + strconv.Itoa(record.Slot) + ":" + state, Backup: backup}, nil
}

func stoppedObservation(spec providercontrol.IONOSSharedPoolSpec, reason string) providercontrol.IONOSSharedPoolObservation {
	return providercontrol.IONOSSharedPoolObservation{Spec: spec, State: "stopped", EvidenceRef: "ionos-worker:" + spec.NodeID + ":" + spec.NetworkNamespace + ":stopped:" + reason}
}

func (app Application) samePolicy(other Application) bool {
	return app.Image == other.Image && app.DataPath == other.DataPath && app.UID == other.UID && app.GID == other.GID && slices.Equal(app.HealthCheck, other.HealthCheck)
}

func (runtime *Runtime) provision(ctx context.Context, record *generation, app Application, volume string) error {
	if err := configureStorage(ctx, record, volume); err != nil {
		return err
	}
	if err := os.Chown(volume, app.UID, app.GID); err != nil {
		return err
	}
	if err := runtime.ensureNetwork(ctx, record); err != nil {
		return err
	}
	health, err := json.Marshal(append([]string{"CMD"}, app.HealthCheck...))
	if err != nil {
		return err
	}
	id := record.Spec.NetworkNamespace
	_, err = podman(ctx, "create", "--name", id, "--network", "ns:"+netnsPath(id), "--dns", "1.1.1.1",
		"--pull=missing", "--label", "kombify.generation="+id, "--read-only", "--cap-drop=ALL",
		"--security-opt=no-new-privileges", "--image-volume=ignore", "--pids-limit=128", "--cpus=1", "--log-driver=none",
		"--user", strconv.Itoa(app.UID)+":"+strconv.Itoa(app.GID),
		"--memory", strconv.Itoa(record.Spec.Workload.MemoryMB)+"m", "--memory-swap", strconv.Itoa(record.Spec.Workload.MemoryMB)+"m",
		"--volume", volume+":"+app.DataPath+":rw,nosuid,nodev,noexec", "--health-cmd", string(health),
		"--health-interval=3s", "--health-timeout=2s", "--health-retries=5", app.Image)
	return err
}

type containerState struct {
	Name       string
	OCIRuntime string
	ImageName  string
	State      struct{ Running bool }
	Config     struct {
		Labels map[string]string
		User   string
	}
	HostConfig struct {
		NetworkMode    string
		ReadonlyRootfs bool
		Memory         int64
		Privileged     bool
	}
	Mounts []struct{ Type, Source, Destination string }
}

func (state containerState) matches(id string, app Application, volume string, memoryMB int) bool {
	return state.Name == id && filepath.Base(state.OCIRuntime) == "runsc" && state.ImageName == app.Image &&
		state.Config.Labels["kombify.generation"] == id && state.Config.User == strconv.Itoa(app.UID)+":"+strconv.Itoa(app.GID) && !state.HostConfig.Privileged && state.HostConfig.ReadonlyRootfs &&
		state.HostConfig.NetworkMode == "ns:"+netnsPath(id) && state.HostConfig.Memory == int64(memoryMB)*1024*1024 &&
		len(state.Mounts) == 1 && state.Mounts[0].Type == "bind" && state.Mounts[0].Source == volume && state.Mounts[0].Destination == app.DataPath
}

func inspectContainer(ctx context.Context, id string) (*containerState, error) {
	if _, err := podman(ctx, "container", "exists", id); err != nil {
		var status *exec.ExitError
		if errors.As(err, &status) && status.ExitCode() == 1 {
			return nil, nil
		}
		return nil, err
	}
	data, err := podman(ctx, "container", "inspect", id)
	if err != nil {
		return nil, err
	}
	var observed []containerState
	if err := json.Unmarshal(data, &observed); err != nil || len(observed) != 1 {
		return nil, errors.New("unexpected worker container inspection")
	}
	return &observed[0], nil
}

func waitHealthy(ctx context.Context, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if _, err := podman(ctx, "healthcheck", "run", id); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("worker generation did not become healthy")
		case <-ticker.C:
		}
	}
}

func podman(ctx context.Context, args ...string) ([]byte, error) {
	return command(ctx, "/usr/bin/podman", append([]string{"--runtime", runscPath, "--runtime-flag", "network=host"}, args...)...)
}

func command(ctx context.Context, binary string, args ...string) ([]byte, error) {
	return commandInput(ctx, nil, binary, args...)
}

func commandInput(ctx context.Context, input []byte, binary string, args ...string) ([]byte, error) {
	process := exec.CommandContext(ctx, binary, args...)
	process.Stdin = bytes.NewReader(input)
	output, err := process.Output()
	if err != nil {
		return nil, fmt.Errorf("%s operation failed: %w", filepath.Base(binary), err)
	}
	return output, nil
}

func readGeneration(id string) (*generation, error) {
	data, err := os.ReadFile(filepath.Join(dataDirectory, "generations", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var record generation
	if err := json.Unmarshal(data, &record); err != nil || record.Spec.NetworkNamespace != id || record.App.Image != record.Spec.Workload.Image || record.Slot < 1 || record.Slot > 40 {
		return nil, errors.New("worker generation record is invalid")
	}
	return &record, nil
}

func writeGeneration(record *generation) error {
	directory := filepath.Join(dataDirectory, "generations")
	file, err := os.CreateTemp(directory, ".generation-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := json.NewEncoder(file).Encode(record); err != nil {
		_ = file.Close()
		return err
	}
	syncErr := file.Sync()
	if err := errors.Join(syncErr, file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(directory, record.Spec.NetworkNamespace+".json")); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (runtime *Runtime) reserve(spec providercontrol.IONOSSharedPoolSpec, app Application) (*generation, error) {
	entries, err := os.ReadDir(filepath.Join(dataDirectory, "generations"))
	if err != nil {
		return nil, err
	}
	slots := make(map[int]bool)
	memory := runtime.node.ReservedMemoryMB + spec.Workload.MemoryMB
	for _, entry := range entries {
		id, matches := strings.CutSuffix(entry.Name(), ".json")
		if !matches || !name.MatchString(id) {
			continue
		}
		record, err := readGeneration(id)
		if err != nil || record == nil || record.Spec.NodeID != runtime.node.ID {
			return nil, errors.New("worker cannot reconcile generation allocation")
		}
		if record.Spec.Workload.Handle == spec.Workload.Handle {
			return nil, errors.New("worker requires a globally unique generation handle; handles cannot be reused")
		}
		if !record.Removed {
			if slots[record.Slot] || record.Slot > runtime.node.Slots || hostLink(id) == hostLink(spec.NetworkNamespace) {
				return nil, errors.New("worker network allocation collision")
			}
			slots[record.Slot] = true
			memory += record.Spec.Workload.MemoryMB
		}
	}
	if memory <= runtime.node.MemoryMB {
		for slot := 1; slot <= runtime.node.Slots; slot++ {
			if !slots[slot] {
				record := &generation{Spec: spec, App: app, Slot: slot}
				return record, writeGeneration(record)
			}
		}
	}
	return nil, errors.New("worker capacity exhausted")
}

func netnsPath(id string) string { return filepath.Join("/run/netns", id) }
func hostLink(id string) string  { return "kw" + id[4:17] }

func confirmAbsent(ctx context.Context, spec providercontrol.IONOSSharedPoolSpec, volume string) (providercontrol.IONOSSharedPoolObservation, error) {
	container, err := inspectContainer(ctx, spec.NetworkNamespace)
	if err != nil || container != nil {
		return providercontrol.IONOSSharedPoolObservation{}, errors.New("worker container absence unconfirmed")
	}
	for _, filename := range []string{filepath.Dir(volume), netnsPath(spec.NetworkNamespace), filepath.Join("/sys/class/net", hostLink(spec.NetworkNamespace))} {
		if _, err := os.Lstat(filename); !errors.Is(err, os.ErrNotExist) {
			return providercontrol.IONOSSharedPoolObservation{}, errors.New("worker isolation resource absence unconfirmed")
		}
	}
	return providercontrol.IONOSSharedPoolObservation{Spec: spec, State: "absent", IsolationRemoved: true, EvidenceRef: "ionos-worker:" + spec.NodeID + ":" + spec.NetworkNamespace + ":absent"}, nil
}

var _ providercontrol.IONOSSharedPoolRuntime = (*Runtime)(nil)

func (runtime *Runtime) VerifyIONOSSharedPoolBackup(ctx context.Context, manifest providercontrol.IONOSSharedPoolBackupManifest) error {
	return runtime.backup.VerifyIONOSSharedPoolBackup(ctx, manifest)
}
