package routes

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/kombifyio/homelab/internal/wikipush"
	"github.com/kombifyio/homelab/internal/wikisource"
	"github.com/kombifyio/homelab/pkg/controlplane"
	"github.com/kombifyio/homelab/pkg/httpx"
)

// Wiki source routes (platform-m09kf.5.11). The Homelab Runtime reports its
// own state and its completed change receipts as the sources a Space Wiki
// refresh reads: servers, apps and their versions, addresses, storage,
// backups and who changed what. The document is neutral state; the AI
// Platform maps it to claims, and only the person's own Wiki receives the
// tenant's failure occurrences (D-2026-10-26-07).
//
// GET /api/v1/wiki/sources answers the owner-scoped document. POST
// /api/v1/wiki/report builds the same document and delivers it to the
// configured Space with a short-lived Ed25519 service-call token.
const (
	wikiSourcesPath = "/api/v1/wiki/sources"
	wikiReportPath  = "/api/v1/wiki/report"
	wikiJobPageSize = 200
	wikiTokenTTL    = 2 * time.Minute
)

// WikiSourceConfig configures the report delivery. An empty origin or space
// keeps the report route fail-closed (503) and leaves the read route
// available, so a deployment that reports nowhere still exposes its state.
type WikiSourceConfig struct {
	// Origin is the agent host origin, for example https://agents.kombify.io.
	Origin string
	// Space is the Wiki Space the person's Homelab reports into.
	Space string
}

func (c WikiSourceConfig) enabled() bool {
	return strings.TrimSpace(c.Origin) != "" && strings.TrimSpace(c.Space) != ""
}

func (h inventoryHandlers) httpWikiSources(e *httpx.Event) error {
	if err := rejectInventoryScopeOverrides(e, nil); err != nil {
		return writeInventoryHTTPError(e, err)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	document, err := h.app.wikiDocument(e.Request.Context(), scope, h.jobs)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusOK, document)
}

func (h inventoryHandlers) httpWikiReport(e *httpx.Event) error {
	if err := rejectInventoryScopeOverrides(e, nil); err != nil {
		return writeInventoryHTTPError(e, err)
	}
	scope, err := inventoryScopeFromEvent(e)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	if !h.wiki.enabled() {
		return httpx.Error(e, http.StatusServiceUnavailable, "wiki_report_unconfigured", "Wiki reporting is not configured",
			map[string]any{"reason_code": "wiki_report_unconfigured"})
	}
	document, err := h.app.wikiDocument(e.Request.Context(), scope, h.jobs)
	if err != nil {
		return writeInventoryHTTPError(e, err)
	}
	client, err := wikipush.New(h.wiki.Origin, h.wiki.Space, "", wikiTokenTTL)
	if err != nil {
		return httpx.Error(e, http.StatusServiceUnavailable, "wiki_report_unavailable", "Wiki reporting is unavailable",
			map[string]any{"reason_code": "wiki_report_unavailable"})
	}
	delivery, err := client.Report(e.Request.Context(), scope.ownerID, scope.tenantID, document)
	if err != nil {
		httpx.RecordDependencyFailure(e, "wiki.report", err)
		return httpx.Error(e, http.StatusBadGateway, "wiki_report_refused", "The Wiki refused this source document",
			map[string]any{"reason_code": "wiki_report_refused", "delivery": delivery})
	}
	e.Response.Header().Set("Cache-Control", "private, no-store")
	return httpx.Success(e, http.StatusAccepted, map[string]any{
		"accepted": true, "space": delivery.Space, "sources": len(document.Sources), "skipped": document.Skipped,
	})
}

// wikiDocument reads the owner's servers, their apps, backups and completed
// change receipts, all under the policy-issued owner scope, and projects them
// into one neutral document.
func (a *inventoryApplication) wikiDocument(ctx context.Context, scope inventoryScope, jobs controlplane.JobStore) (wikisource.Document, error) {
	readScope, err := a.authorize(ctx, scope, InventoryActionRead, controlplane.InventoryReadTargetServerCollection, "")
	if err != nil {
		return wikisource.Document{}, err
	}
	now := a.now().UTC()
	servers, stackIDs, err := a.wikiServers(ctx, readScope)
	if err != nil {
		return wikisource.Document{}, err
	}
	services, err := a.wikiServices(ctx, readScope, servers)
	if err != nil {
		return wikisource.Document{}, err
	}
	backups, failures, err := a.wikiBackups(ctx, readScope, now)
	if err != nil {
		return wikisource.Document{}, err
	}
	return wikisource.Build(wikisource.Input{
		OwnerSubjectID: scope.ownerID,
		GeneratedAt:    now,
		Servers:        servers,
		Services:       services,
		Backups:        backups,
		Receipts:       wikiReceipts(ctx, scope.tenantID, stackIDs, jobs),
		Failures:       failures,
	}), nil
}

func (a *inventoryApplication) wikiServers(ctx context.Context, readScope controlplane.InventoryReadScope) ([]wikisource.Server, map[string]struct{}, error) {
	document := make([]wikisource.Server, 0, 16)
	stackIDs := map[string]struct{}{}
	page := controlplane.InventoryPageRequest{Limit: controlplane.MaxInventoryPageSize}
	for len(document) < wikisource.MaxServers {
		result, err := a.read.ListInventoryServers(ctx, readScope, page)
		if err != nil {
			return nil, nil, inventoryStoreError(err)
		}
		for _, server := range result.Servers {
			if len(document) >= wikisource.MaxServers {
				break
			}
			if stackID := strings.TrimSpace(server.StackID); stackID != "" {
				stackIDs[stackID] = struct{}{}
			}
			projected := wikisource.Server{
				ID:              safeLabel(server.ID, 256),
				Name:            safeLabel(firstNonEmptyString(server.DisplayName, server.Name, server.ID), 256),
				OperatingSystem: safeLabel(stringFromAnyMap(safeAnyMap(server.Metadata["host"]), "os"), 64),
				OSVersion:       safeLabel(stringFromAnyMap(safeAnyMap(server.Metadata["host"]), "os_version"), 64),
				Lifecycle:       safeState(server.LifecycleState, monitoringStatusUnknown),
			}
			if addresses := wikiServerAddresses(server); len(addresses) > 0 {
				projected.Addresses = addresses
			}
			if disk, ok := wikiServerDiskPercent(server); ok {
				projected.DiskPercent = &disk
			}
			document = append(document, projected)
		}
		if result.Next == nil || result.Next.IsZero() {
			break
		}
		page.After = *result.Next
		page.Watermark = result.Watermark
	}
	return document, stackIDs, nil
}

func (a *inventoryApplication) wikiServices(ctx context.Context, readScope controlplane.InventoryReadScope, servers []wikisource.Server) ([]wikisource.Service, error) {
	document := make([]wikisource.Service, 0, len(servers)*2)
	for _, server := range servers {
		serverScope, err := readScope.RestrictToServer(server.ID)
		if err != nil {
			continue
		}
		result, err := a.read.ListInventoryServices(ctx, serverScope, server.ID,
			controlplane.InventoryPageRequest{Limit: wikisource.MaxServicesPerServer})
		if err != nil {
			return nil, inventoryStoreError(err)
		}
		for _, service := range result.Services {
			if strings.TrimSpace(service.ServerID) != server.ID {
				continue
			}
			projected := wikisource.Service{
				ID:       safeLabel(service.ID, 256),
				Key:      safeLabel(service.ServiceKey, 128),
				Name:     safeLabel(firstNonEmptyString(service.Name, service.ServiceKey, service.ID), 256),
				ServerID: server.ID,
				Version:  safeLabel(wikiServiceVersion(service), 96),
				Observed: safeState(service.ObservedState, monitoringStatusUnknown),
			}
			if projected.Name == "" {
				continue
			}
			if service.ObservedAt != nil {
				projected.ObservedAt = service.ObservedAt.UTC().Format(time.RFC3339)
			}
			if hostname, port := wikisource.LinkShape(wikiServiceURL(service)); hostname != "" {
				projected.Hostnames = append(projected.Hostnames, hostname)
				if port > 0 {
					projected.Ports = append(projected.Ports, port)
				}
			}
			document = append(document, projected)
		}
	}
	return document, nil
}

func (a *inventoryApplication) wikiBackups(ctx context.Context, readScope controlplane.InventoryReadScope, now time.Time) ([]wikisource.BackupState, []wikisource.Failure, error) {
	reader, ok := a.read.(controlplane.InventoryMonitoringReader)
	if !ok {
		return nil, nil, nil
	}
	facts, err := reader.ListInventoryBackupFacts(ctx, readScope, now)
	if err != nil {
		return nil, nil, inventoryStoreError(err)
	}
	backups := make([]wikisource.BackupState, 0, len(facts))
	failures := make([]wikisource.Failure, 0, 4)
	for _, fact := range facts {
		projected := wikisource.Backup{}
		if fact.OffsiteConfigured {
			projected.Target = "the managed off-site store"
		}
		if fact.Schedule != nil && fact.Schedule.LastRunAt != nil {
			projected.LastRunAt = fact.Schedule.LastRunAt.UTC().Format(time.RFC3339)
		}
		for _, run := range fact.Runs {
			at := run.At.UTC().Format(time.RFC3339)
			if projected.LastRunAt == "" || at > projected.LastRunAt {
				projected.LastRunAt = at
			}
			if run.State == "failed" {
				projected.State = "failed"
				projected.LastResult = "failed"
				if reason := strings.TrimSpace(firstNonEmptyString(run.Error, run.Step)); reason != "" {
					failures = append(failures, wikisource.Failure{
						ServerID: strings.TrimSpace(fact.ServerID), Step: safeLabel(run.Step, 128), Reason: safeLabel(reason, 200), Count: 1, LastAt: at,
					})
				}
				continue
			}
			if run.State == "completed" && projected.State != "failed" {
				projected.State = "completed"
				projected.LastResult = "completed"
			}
		}
		backups = append(backups, wikisource.BackupState{ServerID: strings.TrimSpace(fact.ServerID), Backup: projected})
	}
	return backups, failures, nil
}

// wikiReceipts reads the completed typed StackKits jobs of the owner's stacks
// and projects the application update outcome of each. Who asked is the job's
// recorded actor, never an inference from the current session.
func wikiReceipts(ctx context.Context, tenantID string, stackIDs map[string]struct{}, jobs controlplane.JobStore) []wikisource.Receipt {
	if jobs == nil || len(stackIDs) == 0 {
		return nil
	}
	rows, err := jobs.ListJobsByTenant(ctx, tenantID, wikiJobPageSize)
	if err != nil {
		return nil
	}
	receipts := make([]wikisource.Receipt, 0, 8)
	for _, job := range rows {
		if strings.TrimSpace(job.Type) != "stackkit_lifecycle" || job.CompletedAt == nil {
			continue
		}
		if _, owned := stackIDs[strings.TrimSpace(job.StackID)]; !owned {
			continue
		}
		receipt := wikiReceiptOf(job)
		if receipt.ID == "" {
			continue
		}
		receipts = append(receipts, receipt)
	}
	return receipts
}

func wikiReceiptOf(job controlplane.Job) wikisource.Receipt {
	target := wikisource.Receipt{
		ID:    safeLabel(job.ID, 128),
		At:    job.CompletedAt.UTC().Format(time.RFC3339),
		Actor: wikiJobActor(job.Payload),
	}
	service := safeAnyMap(job.Result["service_action_receipt"])
	target.ServiceID = safeLabel(stringFromAnyMap(service, "service_id"), 256)
	target.ServiceKey = safeLabel(stringFromAnyMap(service, "service_key"), 128)
	if action := stringFromAnyMap(service, "action"); action != "" {
		target.Action = safeLabel(action, 64)
	}
	if outcome := safeAnyMap(job.Result["application_update_outcome"]); len(outcome) > 0 {
		target.Action = "application_update"
		target.Result = safeLabel(stringFromAnyMap(outcome, "result"), 32)
		target.PriorVersion = safeLabel(stringFromAnyMap(safeAnyMap(outcome["prior"]), "version"), 96)
		target.TargetVersion = safeLabel(stringFromAnyMap(safeAnyMap(outcome["target"]), "version"), 96)
		if receiptReason := stringFromAnyMap(outcome, "reason_code"); receiptReason != "" {
			target.Summary = "the node reported " + safeReasonToken(receiptReason)
		}
	}
	if target.Action == "" {
		return wikisource.Receipt{}
	}
	if target.Result == "" {
		target.Result = "completed"
	}
	if target.Summary == "" && target.TargetVersion != "" {
		target.Summary = target.Action + " to " + target.TargetVersion
	}
	return target
}

func wikiJobActor(payload map[string]any) string {
	actor := safeAnyMap(payload["actor"])
	if email := stringFromAnyMap(actor, "email"); email != "" {
		return safeLabel(email, 128)
	}
	return safeLabel(stringFromAnyMap(actor, "user_id"), 128)
}

func wikiServiceVersion(service controlplane.ServiceRuntime) string {
	if connector := safeAnyMap(service.Metadata["application_connector"]); len(connector) > 0 {
		if version := stringFromAnyMap(connector, "application_version"); version != "" {
			return version
		}
	}
	return service.StackKitVersion
}

func wikiServiceURL(service controlplane.ServiceRuntime) string {
	raw := stringFromAnyMap(safeAnyMap(service.Access), serviceAccessURLKey)
	if raw == "" {
		return ""
	}
	sanitized, ok := sanitizedInventoryServiceURL(raw)
	if !ok {
		return ""
	}
	return sanitized
}

func wikiServerAddresses(server controlplane.ServerRuntime) []string {
	host := safeAnyMap(server.Metadata["host"])
	addresses := inventoryAddressesFromMetadata(server.Metadata, host)
	out := make([]string, 0, 4)
	for _, candidate := range append(append(append([]string{}, addresses.PublicIPs...), addresses.PrivateIPs...), addresses.LocalIPs...) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			continue
		}
		out = append(out, safeLabel(candidate, 64))
		if len(out) >= 4 {
			break
		}
	}
	return out
}

func wikiServerDiskPercent(server controlplane.ServerRuntime) (float64, bool) {
	value, ok := safeAnyMap(server.Metadata["metrics"])["disk_percent"].(float64)
	if !ok || value < 0 || value > 100 {
		return 0, false
	}
	return value, true
}
