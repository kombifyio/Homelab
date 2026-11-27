// Package routes provides the Unifier API endpoints.
package routes

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/kombifyio/homelab/pkg/config"
	"github.com/kombifyio/homelab/pkg/controlplane"
	kscore "github.com/kombifyio/homelab/pkg/core"
	"github.com/kombifyio/homelab/pkg/features"
	"github.com/kombifyio/homelab/pkg/httpx"
	"github.com/kombifyio/homelab/pkg/unifier"
)

const maxUnifierRequestBodyBytes int64 = 2 << 20 // 2 MiB

func readRequestBodyLimited(r io.Reader, limitBytes int64) ([]byte, bool, error) {
	limited := io.LimitReader(r, limitBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	if int64(len(body)) > limitBytes {
		return nil, true, nil
	}
	return body, false, nil
}

// UnifierAPI holds the Unifier engine and pipeline instances.
type UnifierAPI struct {
	engine             *unifier.Engine
	pipeline           *unifier.Pipeline
	loader             *unifier.Loader
	recommendations    *unifier.WizardRecommendationAuthority
	workers            controlplane.WorkerStore
	features           *features.Service
	deploymentMode     config.DeploymentMode
	memoryAvailability interface{ IONOSStandardMemoryAvailable(context.Context) error }
	selfDisclosures    controlplane.OperatorSelfDisclosureStore
	// servers and transitions are the canonical server rows and their
	// connection history. They are the same store that backs the worker
	// registry; without them workers carry no server identity or availability
	// evidence.
	servers     controlplane.ServerRuntimeStore
	services    controlplane.ServiceRuntimeStore
	transitions serverTransitionWindowReader
	// now is the clock the evidence windows and freshness read; nil is the
	// wall clock.
	now func() time.Time
}

func (api *UnifierAPI) clock() time.Time {
	if api.now != nil {
		return api.now().UTC()
	}
	return time.Now().UTC()
}

// NewUnifierAPI creates a new UnifierAPI instance with full pipeline support.
func NewUnifierAPI(workers controlplane.WorkerStore) (*UnifierAPI, error) {
	engine, err := unifier.New()
	if err != nil {
		return nil, err
	}

	pipeline := unifier.NewPipeline(engine)

	// The control-plane store that backs the worker registry also persists
	// operator self-disclosures; both are tenant-scoped tables of one database.
	selfDisclosures, _ := workers.(controlplane.OperatorSelfDisclosureStore)
	servers, _ := workers.(controlplane.ServerRuntimeStore)
	services, _ := workers.(controlplane.ServiceRuntimeStore)
	transitions, _ := workers.(serverTransitionWindowReader)

	return &UnifierAPI{
		engine:          engine,
		pipeline:        pipeline,
		loader:          unifier.NewLoader(),
		recommendations: unifier.NewWizardRecommendationAuthority(engine),
		workers:         workers,
		selfDisclosures: selfDisclosures,
		servers:         servers,
		services:        services,
		transitions:     transitions,
	}, nil
}

func requireUnifierAuth(e *httpx.Event) (string, error) {
	if userID, ok := authenticatedUserID(e); ok {
		return userID, nil
	}
	return "", httpx.RejectUnauthorized(e, "Authentication required")
}

// fetchWorkers loads the exact owner's workers from the canonical tenant store.
func (api *UnifierAPI) fetchWorkers(ctx context.Context, tenantID, ownerID string) ([]kscore.Worker, error) {
	if api.workers == nil {
		return nil, fmt.Errorf("unifier: worker store is not configured")
	}
	records, err := api.workers.ListWorkersByTenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	return api.unifierWorkers(ctx, tenantID, ownerRecords(records, ownerID), api.clock()), nil
}

// ownerRecords keeps the records of the exact owner.
func ownerRecords(records []controlplane.Worker, ownerID string) []controlplane.Worker {
	owned := make([]controlplane.Worker, 0, len(records))
	for _, record := range records {
		if strings.TrimSpace(record.OwnerSubjectID) == strings.TrimSpace(ownerID) {
			owned = append(owned, record)
		}
	}
	return owned
}

const wizardRecommendationInventoryTTL = 10 * time.Minute

// fetchRecommendationInventory reads the same tenant worker registry as the
// ordinary Unifier path, then applies the authenticated owner filter before
// exposing any inventory to the recommendation authority.
func (api *UnifierAPI) fetchRecommendationInventory(ctx context.Context, tenantID, ownerID string) (unifier.WizardRecommendationInventory, error) {
	inventory := unifier.WizardRecommendationInventory{
		State:      "unavailable",
		Source:     "worker-registry",
		SnapshotID: "worker-registry",
	}
	if api.workers == nil {
		return inventory, fmt.Errorf("unifier: worker store is not configured")
	}
	records, err := api.workers.ListWorkersByTenant(ctx, tenantID)
	if err != nil {
		return inventory, err
	}
	inventory.State = "fresh"
	inventory.StaleWorkerIDs = make(map[string]bool)
	stale := false
	var latest time.Time
	now := api.clock()
	owned := ownerRecords(records, ownerID)
	inventory.Workers = api.unifierWorkers(ctx, tenantID, owned, now)
	for _, record := range owned {
		if record.LastSeenAt == nil || now.Sub(record.LastSeenAt.UTC()) > wizardRecommendationInventoryTTL {
			stale = true
			inventory.StaleWorkerIDs[record.ID] = true
		}
		if record.LastSeenAt != nil && record.LastSeenAt.After(latest) {
			latest = record.LastSeenAt.UTC()
		}
	}
	if stale {
		inventory.State = "stale"
		inventory.StaleInputs = []string{"worker_inventory"}
	}
	inventory.ObservedAt = latest
	return inventory, nil
}

func unifierWorkerTags(values map[string]any) map[string]string {
	tags := make(map[string]string, len(values))
	for key, value := range values {
		if text, ok := value.(string); ok && strings.TrimSpace(key) != "" {
			tags[strings.TrimSpace(key)] = strings.TrimSpace(text)
		}
	}
	return tags
}

// RegisterUnifierRoutes adds the /api/v1/unifier/* endpoints.
func RegisterUnifierRoutes(r *httpx.Router, workers controlplane.WorkerStore, featureService *features.Service, memoryAvailability interface{ IONOSStandardMemoryAvailable(context.Context) error }, deploymentMode config.DeploymentMode) error {
	api, err := NewUnifierAPI(workers)
	if err != nil {
		return err
	}

	api.features = featureService
	api.deploymentMode = deploymentMode
	api.memoryAvailability = memoryAvailability
	r.POST("/api/v1/unifier/validate", func(e *httpx.Event) error { return api.handleValidate(e) })
	r.POST("/api/v1/unifier/unify", func(e *httpx.Event) error { return api.handleUnify(e) })
	r.POST("/api/v1/unifier/recommendations", func(e *httpx.Event) error { return api.handleRecommendations(e) })
	r.GET("/api/v1/operator/self-disclosure", func(e *httpx.Event) error { return api.handleGetSelfDisclosure(e) })
	r.PUT("/api/v1/operator/self-disclosure", func(e *httpx.Event) error { return api.handlePutSelfDisclosure(e) })
	// The same tenant-scoped database keeps the owner's creation drafts.
	drafts, _ := workers.(controlplane.CreationDraftStore)
	registerCreationDraftRoutes(r, drafts)
	r.POST("/api/v1/unifier/pipeline", func(e *httpx.Event) error { return api.handlePipeline(e) })
	r.POST("/api/v1/unifier/pipeline/validate", func(e *httpx.Event) error { return api.handlePipeline(e) })
	r.POST("/api/v1/unifier/pipeline/preview", func(e *httpx.Event) error { return api.handlePipelinePreview(e) })
	r.POST("/api/v1/unifier/generate", func(e *httpx.Event) error { return api.handleGenerate(e) })

	r.GET("/api/v1/stackkits", func(e *httpx.Event) error { return api.handleListStackKits(e) })
	// Registered before the {name} pattern so the literal path wins.
	r.GET("/api/v1/stackkits/use-cases", func(e *httpx.Event) error { return api.handleUseCaseCatalog(e) })
	r.GET("/api/v1/stackkits/{name}", func(e *httpx.Event) error { return api.handleGetStackKit(e) })

	r.GET("/api/v1/addons", func(e *httpx.Event) error { return api.handleListAddons(e) })
	r.POST("/api/v1/addons/detect", func(e *httpx.Event) error { return api.handleDetectAddons(e) })

	r.POST("/api/v1/unifier/analyze", func(e *httpx.Event) error { return api.handleAnalyze(e) })
	r.POST("/api/v1/unifier/iac", func(e *httpx.Event) error { return api.handleIaCGeneration(e) })
	r.POST("/api/v1/unifier/iac/preview", func(e *httpx.Event) error { return api.handleIaCPreview(e) })

	return nil
}
