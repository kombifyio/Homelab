<script lang="ts">
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import {
    tr,
    trn,
    stateLabel,
    formatDate,
    type MessageKey,
  } from "#lib/i18n.svelte.js";
  import { onMount } from "svelte";
  import { MediaQuery } from "svelte/reactivity";
  import { goto, pushState, replaceState } from "$app/navigation";
  import { page } from "$app/state";
  import {
    listServiceRegistry,
    deleteRegistryService,
    type RegistryServer,
    type RegistryService,
    type ServiceRegistryPayload,
  } from "#lib/api/registry.js";
  import { getMonitoringCockpit } from "#lib/api/monitoring.js";
  import {
    applyCockpitRefreshError,
    applyCockpitRefreshSuccess,
    beginCockpitRefresh,
    type CockpitRefreshState,
  } from "#lib/monitoring/cockpit-snapshot.js";
  import { getJob, type JobDetail } from "#lib/api/jobs.js";
  import { parseApiError } from "#lib/api/errors.js";
  import {
    applicationMoveOutcome,
    startApplicationMove,
    type ApplicationMoveRequest,
  } from "#lib/api/applicationMove.js";
  import {
    clearExtendWizardIdempotencyKey,
    mintExtendWizardIdempotencyKey,
  } from "#lib/wizard/idempotency-keys.js";
  import {
    blockedMoveTargetsFor as blockedMoveTargetsAmong,
    cockpitServerFor,
    ineligibleDestinationsByServer,
    migrationProgress,
    moveTargetsFor as moveTargetsAmong,
    serverCapacity,
    serviceAccessEnabled as serviceAccessEnabledFor,
    serviceMeta,
    unmanagedOn,
    type PlacementColumn,
    type ServerCapacity,
    type UnmanagedCard,
  } from "#lib/services/placement.js";
  import {
    listCanonicalServicesReport,
    listServiceApplicationsReport,
    type CanonicalService,
    type ServiceApplication,
    type ServiceRowRejection,
  } from "#lib/api/services.js";
  import {
    openServiceUrl,
    serviceCardLogo,
    serviceCardLogoDomain,
    serviceCardLogoSrc,
    serviceCardUpdate,
    serviceCardVersionNotice,
    serviceCardUpdateClass,
    serviceCardVersion,
    serviceIsLocked,
    serviceLockReason,
    serviceCardPlacement as resolveServiceCardPlacement,
    serviceCardStatus as resolveServiceCardStatus,
    serviceCardStatusMessage as resolveServiceCardStatusMessage,
    serviceHasActiveMigration,
    serviceManagementState,
    type GovernedServiceAction,
  } from "#lib/service-card-adapter.js";
  import {
    ServiceActionTracker,
    actionFailed,
    isActionActive,
  } from "#lib/services/service-action-tracker.svelte.js";
  import {
    actionLabel,
    adoptionUnavailableReason,
    applicationCardActions,
    componentCardActions,
    runApplicationAction,
    runComponentAction,
    type ApplicationAction,
  } from "#lib/services/application-actions.js";
  import {
    applicationLocked,
    applicationOwnership,
    applicationPlacement,
    applicationVersion,
    runtimePlacement,
  } from "#lib/services/application-card.js";
  import ServiceList, {
    type ServiceListGroup,
    type ServiceListItem,
  } from "#lib/components/inventory/ServiceList.svelte";
  import ServiceMoveDialog from "#lib/components/inventory/ServiceMoveDialog.svelte";
  import TrademarkNotice from "#lib/components/TrademarkNotice.svelte";
  import PlacementBoard from "#lib/components/inventory/PlacementBoard.svelte";
  import PlacementClassBoard from "#lib/components/inventory/PlacementClassBoard.svelte";
  import {
    columnsWithoutLifted,
    partitionPlacementBoard,
  } from "#lib/services/placement-classes.js";
  import {
    PLACEMENT_BOARD_CLASSES_FLAG,
    features,
    getDefaultEnabled,
  } from "#lib/stores/features.js";
  import ServiceAdoptionDialog from "#lib/components/inventory/ServiceAdoptionDialog.svelte";
  import StackItDialog from "#lib/components/inventory/StackItDialog.svelte";
  import { confirmInApp } from "#lib/dialogs/in-app-dialog.js";
  import {
    ServiceDetailSheet,
    type ServiceCardAction,
    type ServiceDetailTab,
    type ServiceMigration,
    type ServicePlacement,
    type ServiceStatusKind,
  } from "@kombiverselabs/ui/service";
  import { PageHeader } from "@kombiverselabs/ui/shell";
  import Button from "#lib/components/ui/Button.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import { SegmentedControl } from "@kombiverselabs/ui/primitives";
  import {
    AlertTriangle,
    Trash2,
    Loader2,
  } from "@lucide/svelte";

  const emptyRegistry: ServiceRegistryPayload = {
    catalog: [],
    kit_deployments: [],
    servers: [],
    services: [],
  };

  let registry = $state<ServiceRegistryPayload>(emptyRegistry);
  let canonicalServices = $state<CanonicalService[]>([]);
  let serviceApplications = $state<ServiceApplication[]>([]);
  let loading = $state(true);
  let inventoryLoading = $state(true);
  let saving = $state(false);
  let registryError = $state<string | null>(null);
  let inventoryError = $state<string | null>(null);
  let actionError = $state<string | null>(null);
  /** Rows the client could not read; the rest of the list still renders. */
  let inventoryRejected = $state<ServiceRowRejection[]>([]);
  const tracker = new ServiceActionTracker({
    refresh: () => refreshRegistryQuietly(),
    observedRevision: (serviceId) =>
      canonicalServices.find((service) => service.id === serviceId)
        ?.inventory_revision,
  });
  let servicesPageActive = false;
  let statusFilter = $state<string>("all");
  let adoptionService = $state<CanonicalService | null>(null);
  let releaseAdoption = $state(false);
  /** "Stack it" of an unmanaged container, or an action on its kept original. */
  let stackIt = $state<{
    service: CanonicalService;
    mode: "stack" | "restore_original" | "release_original";
  } | null>(null);
  let selectedKitDeploymentId = $state("");
  let selectedServerId = $state("");

  // Sub-tabs
  let activeTab = $state<"services" | "servers">("services");

  // Only the page head differs below the shell's phone breakpoint (48rem):
  // the view switch is one row under the title. Everything below the head is
  // the same at every width.
  const wide = new MediaQuery("min-width: 48rem");
  // Service detail sheet — driven by ?service={id} shallow routing so it
  // deep-links, the back button closes it, and board state survives.
  let detailTab = $state<ServiceDetailTab>("overview");
  /**
   * The sheet's own state, kept in step with the URL rather than derived from
   * it: `page.url` does not follow a shallow `pushState` in this SvelteKit
   * version, so a derivation off it renders a deep link correctly and then
   * never reacts to a click. Open/close set this directly and popstate re-reads
   * the address bar, so deep link, click and back all agree.
   */
  let detailServiceId = $state<string | null>(null);

  function serviceIdFromLocation(): string | null {
    if (typeof location === "undefined") return null;
    return new URL(location.href).searchParams.get("service");
  }

  /** `?service=<id>&sheet=logs` deep-links straight into one sheet tab. */
  function sheetTabFromLocation(): ServiceDetailTab {
    if (typeof location === "undefined") return "overview";
    const sheet = new URL(location.href).searchParams.get("sheet");
    return sheet === "activity" || sheet === "logs" || sheet === "operations"
      ? sheet
      : "overview";
  }

  /** `?service=<id>&adopt=1` opens "Stack it" for an observed component. */
  function openAdoptionFromLocation() {
    if (!servicesPageActive) return;
    const url = new URL(location.href);
    if (url.searchParams.get("adopt") !== "1") return;
    const target = canonicalServices.find(
      (service) => service.id === url.searchParams.get("service"),
    );
    url.searchParams.delete("adopt");
    replaceState(url, {});
    if (target?.allowed_actions.includes("adopt")) {
      openAdoptionDialog(target, false);
    }
  }

  function syncServiceSheetFromLocation() {
    detailServiceId = serviceIdFromLocation();
    detailTab = sheetTabFromLocation();
  }

  function openServiceSheet(
    serviceId: string,
    tab: ServiceDetailTab = "overview",
  ) {
    detailTab = tab;
    detailServiceId = serviceId;
    const url = new URL(page.url.href);
    url.searchParams.set("service", serviceId);
    pushState(url, {});
  }

  function closeServiceSheet() {
    if (!detailServiceId) return;
    detailServiceId = null;
    const url = new URL(page.url.href);
    url.searchParams.delete("service");
    url.searchParams.delete("sheet");
    pushState(url, {});
  }

  function openAdoptionDialog(service: CanonicalService, release: boolean) {
    closeServiceSheet();
    // Every unmanaged container except Home Assistant (in place) is stacked
    // as an own application.
    if (!release && service.adoption?.stack_it) {
      stackIt = { service, mode: "stack" };
      return;
    }
    adoptionService = service;
    releaseAdoption = release;
  }

  function openStackedOriginal(
    service: CanonicalService,
    mode: "restore_original" | "release_original",
  ) {
    closeServiceSheet();
    stackIt = { service, mode };
  }

  function closeStackIt() {
    const service = stackIt?.service;
    stackIt = null;
    if (service) openServiceSheet(service.id, "operations");
  }

  function closeAdoptionDialog() {
    const service = adoptionService;
    adoptionService = null;
    if (service) openServiceSheet(service.id, "operations");
  }

  /**
   * Owner approval is a real gate, not an implied checkbox: every governed
   * mutation asks before the POST creates its governed action card.
   */
  async function requestServiceAction(
    service: CanonicalService,
    action: GovernedServiceAction,
  ) {
    actionError = null;
    if (action === "logs") openServiceSheet(service.id, "logs");
    const error = await runComponentAction(tracker, service, action);
    if (error) actionError = error;
  }

  function cardActionsForRuntime(
    runtime: CanonicalService,
    accessUrl: string | undefined,
  ): ServiceCardAction[] {
    return componentCardActions(runtime, tracker, {
      onAction: (action) => void requestServiceAction(runtime, action),
      onAdopt: () => openAdoptionDialog(runtime, false),
      onOpen: accessUrl ? () => openServiceUrl({ url: accessUrl }) : undefined,
      onDetails: () => openServiceSheet(runtime.id),
    });
  }

  async function requestApplicationAction(
    application: ServiceApplication,
    action: ApplicationAction,
  ) {
    actionError = null;
    const error = await runApplicationAction(tracker, application, action);
    if (error) actionError = error;
  }

  function cardActionsForApplication(
    application: ServiceApplication,
  ): ServiceCardAction[] {
    return applicationCardActions(application, tracker, {
      onLogs: (target) => void requestServiceAction(target, "logs"),
      onRun: (action) => void requestApplicationAction(application, action),
      onAdopt: (target) => openAdoptionDialog(target, false),
      onOpen: (url) => openServiceUrl({ url }),
      onDetails: () =>
        void goto(
          `/services/${encodeURIComponent(application.application_id)}`,
        ),
    });
  }

  // Native moves started on the Placement Board, keyed by source StackKit
  // and workload. The card presentation is bound to the accepted job; a
  // generic service status alone never turns a card into "moving".
  interface BoardMove {
    stackId: string;
    jobId: string;
    serviceIds: string[];
    job?: JobDetail;
    statusUnavailable: boolean;
  }
  let boardMoves = $state<Record<string, BoardMove>>({});
  let moveSource = $state<RegistryService | null>(null);
  let moveTarget = $state<RegistryServer | null>(null);
  let moveSubmitting = $state(false);
  let moveFailure = $state<string | null>(null);
  let cockpitState = $state<CockpitRefreshState>({
    snapshot: null,
    stackId: "",
    stale: false,
    error: null,
    generation: 0,
  });
  const cockpitStale = $derived(cockpitState.stale);
  const cockpitError = $derived(cockpitState.error);
  const activeCockpit = $derived(
    cockpitState.snapshot?.kit_deployment_id === selectedKitDeploymentId
      ? cockpitState.snapshot
      : null,
  );

  onMount(() => {
    servicesPageActive = true;
    tracker.start();
    const params = new URLSearchParams(window.location.search);
    const kitDeploymentId = params.get("kit_deployment_id");
    const tab = params.get("tab");
    if (kitDeploymentId) selectedKitDeploymentId = kitDeploymentId;
    if (tab === "servers") activeTab = "servers";
    syncServiceSheetFromLocation();
    window.addEventListener("popstate", syncServiceSheetFromLocation);
    void loadServicesPage().then(openAdoptionFromLocation);
    const interval = setInterval(() => {
      if (hasActiveMigrations()) {
        void pollBoardMoves();
        void refreshRegistryQuietly();
      }
      if (tracker.hasActiveJobs() || tracker.hasConverging()) {
        void tracker.tick();
      }
    }, 3000);
    return () => {
      servicesPageActive = false;
      tracker.stop();
      window.removeEventListener("popstate", syncServiceSheetFromLocation);
      clearInterval(interval);
    };
  });

  interface RuntimeServiceRow extends ServiceListItem {
    runtime: CanonicalService;
    registry?: RegistryService;
    runtimeTargetName: string;
  }

  function label(value?: string): string {
    return stateLabel(value);
  }

  function registryServiceForRuntime(
    runtime: CanonicalService,
  ): RegistryService | undefined {
    return registry.services.find((service) => service.id === runtime.id);
  }

  const placementContext = $derived({
    servers: registry.servers,
    kitDeployments: registry.kit_deployments,
  });

  const PLACEMENT_KEYS: Record<ServicePlacement, MessageKey> = {
    cloud: "ui.services.placement.cloud",
    local: "ui.services.placement.local",
    managed: "ui.services.placement.managed",
    serverless: "ui.services.placement.managed",
    unknown: "ui.services.placement.unknown",
  };

  function runtimeServicePlacement(
    runtime: CanonicalService,
    service?: RegistryService,
  ): ServicePlacement {
    return runtimePlacement(runtime, service, placementContext);
  }

  function hasPersistedMigration(service?: RegistryService): boolean {
    return serviceHasActiveMigration({
      migration_status: service?.migration_status,
    });
  }

  function boardMoveFor(serviceId: string | undefined): BoardMove | undefined {
    if (!serviceId) return undefined;
    return Object.values(boardMoves).find((move) =>
      move.serviceIds.includes(serviceId),
    );
  }

  function hasTrackedMigration(serviceId: string): boolean {
    const move = boardMoveFor(serviceId);
    return Boolean(move && !moveJobTerminal(move.job));
  }

  const runtimeServiceRows = $derived.by<RuntimeServiceRow[]>(() =>
    canonicalServices.map((runtime) => {
      const service = registryServiceForRuntime(runtime);
      const server = registry.servers.find(
        (candidate) => candidate.id === runtime.server_id,
      );
      const runtimeState = runtime.health.state || runtime.observed_state;
      const placement = runtimeServicePlacement(runtime, service);
      const cardPlacement: ServicePlacement = placement;
      const management = runtime.management_state;
      const migration =
        hasPersistedMigration(service) || hasTrackedMigration(runtime.id);
      const runtimeTargetId =
        runtime.target_kind === "managed_workload"
          ? runtime.placement.managed_target_ref || "managed-workload:unknown"
          : runtime.server_id || "runtime-target:unknown";
      const needsAttention = [
        "error",
        "failed",
        "unhealthy",
        "offline",
        "unknown",
      ].includes(runtimeState.toLowerCase());
      const cardSource = {
        ...service,
        name: runtime.name,
        display_name: service?.display_name || runtime.name,
        type: service?.type || runtime.service_key,
        status: runtimeState,
        management_state: runtime.management_state,
        server_id: runtime.server_id,
        server_name: server?.name || service?.server_name,
        placement: cardPlacement,
        operation_state: migration ? "running" : undefined,
      };
      // A component keeps its own recognition: the viewer's logo override
      // belongs to the application, not to each of its parts.
      const runtimeLogoDomain = serviceCardLogoDomain(
        runtime.catalog,
        runtime.application_key,
        runtime.service_key,
        runtime.application_display_name,
        service?.display_name || runtime.name,
      );
      const accessUrl =
        service?.url && serviceAccessEnabled(service)
          ? service.url
          : typeof runtime.access.url === "string"
            ? runtime.access.url
            : undefined;
      return {
        id: runtime.id,
        name: service?.display_name || runtime.name,
        meta:
          management === "observed"
            ? tr("ui.services.discoveredOn", {
                name:
                  server?.name ||
                  runtime.server_id ||
                  tr("ui.services.thisHomelab"),
              })
            : `${service?.type || runtime.service_key || tr("ui.services.serviceFallback")} · ${tr(PLACEMENT_KEYS[placement])}`,
        placement: cardPlacement,
        status: resolveServiceCardStatus(cardSource),
        statusLabel: label(runtimeState),
        statusMessage:
          resolveServiceCardStatusMessage(cardSource) ??
          serviceCardVersionNotice(runtime.version),
        runtimeTargetId,
        targetKind: runtime.target_kind,
        targetLabel:
          runtime.target_kind === "managed_workload"
            ? runtime.placement.provider_id ||
              tr("ui.services.managedProviderTarget")
            : server?.name ||
              runtime.server_id ||
              tr("ui.services.placementUnknown"),
        managementLabel:
          management === "managed"
            ? tr("ui.services.stacked")
            : management === "observed"
              ? tr("ui.services.unstacked")
              : tr("ui.services.ownershipUnknown"),
        workflowLabel: migration
          ? label(service?.migration_status || "operation active")
          : undefined,
        freshnessLabel: label(runtime.placement.freshness.state),
        sourceLabel: runtime.source || undefined,
        details: tr("ui.services.desiredObserved", {
          desired: stateLabel(runtime.desired_state),
          observed: stateLabel(runtime.observed_state),
        }),
        logoDomain: runtimeLogoDomain,
        logoSrc: serviceCardLogoSrc(runtimeLogoDomain, {
          catalog: runtime.catalog,
        }),
        version: serviceCardVersion(runtime.version),
        update: serviceCardUpdate(runtime.version),
        updateClass: serviceCardUpdateClass(runtime.version),
        locked: serviceIsLocked(runtime.mutation_lock),
        lockedReason: serviceLockReason(runtime.mutation_lock),
        attention: needsAttention || migration,
        actions: cardActionsForRuntime(runtime, accessUrl),
        desiredState: runtime.desired_state || undefined,
        observedState: runtime.observed_state || undefined,
        runtime,
        registry: service,
        runtimeTargetName:
          server?.name || service?.server_name || runtimeTargetId,
      };
    }),
  );

  const detailRow = $derived(
    detailServiceId
      ? runtimeServiceRows.find((row) => row.id === detailServiceId)
      : undefined,
  );
  /** Stack it state of the open row: its kept original, when stacked. */
  const detailStackIt = $derived(detailRow?.runtime.adoption?.stack_it);
  const detailApplication = $derived(
    detailRow
      ? serviceApplications.find((application) =>
          application.components.some(
            (component) => component.id === detailRow.runtime.id,
          ),
        )
      : undefined,
  );
  /** Board rows may reference registry services without a canonical row yet. */
  const detailRegistryService = $derived(
    detailServiceId
      ? (detailRow?.registry ??
          registry.services.find((service) => service.id === detailServiceId))
      : undefined,
  );
  /** The canonical runtime row, which is where the owner guardrail lives. */
  const detailRuntimeService = $derived(detailRow?.runtime);
  const detailOpen = $derived(
    Boolean(detailServiceId && (detailRow || detailRegistryService)),
  );
  const detailJob = $derived(
    detailServiceId ? tracker.jobs[detailServiceId] : undefined,
  );
  const detailLogState = $derived(
    detailServiceId ? tracker.logs[detailServiceId] : undefined,
  );

  const runtimeSummary = $derived(() => {
    const running = runtimeServiceRows.filter((service) => {
      const state =
        service.runtime.health.state || service.runtime.observed_state;
      return (
        state === "running" || state === "healthy" || state === "reachable"
      );
    }).length;
    const observed = runtimeServiceRows.filter(
      (service) => service.runtime.management_state === "observed",
    ).length;
    const pending = runtimeServiceRows.filter((service) =>
      ["pending", "adopting", "starting"].includes(
        (service.runtime.observed_state || "").toLowerCase(),
      ),
    ).length;
    const error = runtimeServiceRows.filter((service) =>
      ["error", "failed", "unhealthy", "offline"].includes(
        (
          service.runtime.health.state ||
          service.runtime.observed_state ||
          ""
        ).toLowerCase(),
      ),
    ).length;
    return {
      running,
      observed,
      pending,
      error,
      total: runtimeServiceRows.length,
    };
  });

  const filteredRuntimeServiceRows = $derived(
    statusFilter === "all"
      ? runtimeServiceRows
      : statusFilter === "observed"
        ? runtimeServiceRows.filter(
            (service) => service.runtime.management_state === "observed",
          )
        : statusFilter === "running"
          ? runtimeServiceRows.filter((service) =>
              ["running", "healthy", "reachable"].includes(
                (
                  service.runtime.health.state ||
                  service.runtime.observed_state ||
                  ""
                ).toLowerCase(),
              ),
            )
          : statusFilter === "pending"
            ? runtimeServiceRows.filter((service) =>
                ["pending", "adopting", "starting"].includes(
                  (service.runtime.observed_state || "").toLowerCase(),
                ),
              )
            : runtimeServiceRows.filter((service) =>
                ["error", "failed", "unhealthy", "offline"].includes(
                  (
                    service.runtime.health.state ||
                    service.runtime.observed_state ||
                    ""
                  ).toLowerCase(),
                ),
              ),
  );

  const runtimeServiceGroups = $derived.by<ServiceListGroup[]>(() => {
    const groups = new Map<string, ServiceListGroup>();
    for (const service of filteredRuntimeServiceRows) {
      const existing = groups.get(service.runtimeTargetId);
      if (existing) {
        existing.items.push(service);
        continue;
      }
      const server = registry.servers.find(
        (candidate) => candidate.id === service.runtimeTargetId,
      );
      groups.set(service.runtimeTargetId, {
        id: service.runtimeTargetId,
        name: server?.name || service.runtimeTargetName,
        targetKind: service.targetKind,
        meta: server
          ? `${server.role_label} · ${server.hostname || server.id}`
          : service.targetKind === "managed_workload"
            ? tr("ui.services.managedProviderTarget")
            : tr("ui.services.runtimeTarget"),
        items: [service],
      });
    }
    return Array.from(groups.values())
      .map((group) => ({
        ...group,
        items: group.items.sort(
          (left, right) =>
            Number(Boolean(right.attention)) -
              Number(Boolean(left.attention)) ||
            left.name.localeCompare(right.name),
        ),
      }))
      .sort((left, right) => left.name.localeCompare(right.name));
  });

  const RUNNING_STATES = ["healthy", "running", "reachable"];
  const PENDING_STATES = ["pending", "unknown", "adopting", "starting"];
  const ERROR_STATES = ["degraded", "error", "failed", "unhealthy", "offline"];

  function applicationMatchesFilter(application: ServiceApplication): boolean {
    const status = application.status.toLowerCase();
    switch (statusFilter) {
      case "all":
        return true;
      case "running":
        return RUNNING_STATES.includes(status);
      case "pending":
        return PENDING_STATES.includes(status);
      case "observed":
        return ["observed", "mixed"].includes(
          applicationOwnership(application),
        );
      case "error":
        return ERROR_STATES.includes(status);
      default:
        return status === statusFilter;
    }
  }

  const applicationRows = $derived.by<ServiceListItem[]>(() =>
    serviceApplications
      .filter((application) => !application.system)
      .filter(applicationMatchesFilter)
      .map((application) => {
        const server = registry.servers.find(
          (candidate) => candidate.id === application.server_id,
        );
        const version = applicationVersion(application);
        const ownership = applicationOwnership(application);
        const logo = serviceCardLogo(
          application.catalog ?? application.components[0]?.catalog,
          application.application_key,
          [application.application_key, application.display_name],
        );
        const status = resolveServiceCardStatus({ status: application.status });
        const actions = cardActionsForApplication(application);
        const stackable = actions.some((action) => action.id === "adopt");
        const lockedComponent = application.components.find((component) =>
          serviceIsLocked(component.mutation_lock),
        );
        return {
          id: application.application_id,
          name: application.display_name,
          meta: `${trn("ui.services.componentCount", application.components.length)} · ${server?.name || application.server_id}`,
          address: application.access.address,
          addressUnavailableReason: application.access.address
            ? undefined
            : label(application.access.reason || "address unavailable"),
          detailsHref: `/services/${encodeURIComponent(application.application_id)}`,
          placement: applicationPlacement(
            application,
            placementContext,
            registryServiceForRuntime,
          ),
          status,
          statusLabel: label(application.status),
          // The Stack-it strip carries its own hint, and an error strip must
          // not wear an address note.
          statusMessage:
            application.access.kind === "internal" &&
            !stackable &&
            status !== "error"
              ? tr("ui.services.internalAddressOnlyOpenIt")
              : serviceCardVersionNotice(version),
          managementLabel:
            ownership === "managed"
              ? tr("ui.services.stacked")
              : ownership === "observed"
                ? tr("ui.services.unstacked")
                : ownership === "mixed"
                  ? tr("ui.serviceCards.ownershipMixed")
                  : undefined,
          runtimeTargetId: application.server_id,
          targetKind: "server",
          targetLabel: server?.name || application.server_id,
          freshnessLabel:
            application.observed_at || tr("ui.serviceCards.notObserved"),
          sourceLabel: tr("ui.services.stackkitsApplicationModel"),
          logoDomain: logo.domain,
          logoSrc: logo.src,
          version: serviceCardVersion(version),
          update: serviceCardUpdate(version),
          updateClass: serviceCardUpdateClass(version),
          locked: applicationLocked(application),
          lockedReason: serviceLockReason(lockedComponent?.mutation_lock),
          actions,
        } satisfies ServiceListItem;
      }),
  );

  const applicationById = $derived.by(
    () =>
      new Map(
        serviceApplications.map((application) => [
          application.application_id,
          application,
        ]),
      ),
  );

  const applicationGroups = $derived.by<ServiceListGroup[]>(() => {
    const groups = new Map<string, ServiceListGroup>();
    for (const application of applicationRows) {
      const server = registry.servers.find(
        (candidate) => candidate.id === application.runtimeTargetId,
      );
      const group = groups.get(application.runtimeTargetId) || {
        id: application.runtimeTargetId,
        name: server?.name || application.runtimeTargetId,
        meta: server?.hostname || tr("ui.services.nodeBoundApplications"),
        targetKind: "server",
        items: [],
      };
      group.items.push(application);
      groups.set(application.runtimeTargetId, group);
    }
    return Array.from(groups.values());
  });

  const systemServiceGroups = $derived.by<ServiceListGroup[]>(() =>
    (() => {
      // A component of a listed application belongs to that application card,
      // never to the system list; only unclassified technical rows land here.
      const applicationComponentIds = new Set(
        serviceApplications
          .filter((application) => !application.system)
          .flatMap((application) =>
            application.components.map((component) => component.id),
          ),
      );
      const systemIDs = new Set(
        runtimeServiceRows
          .filter(
            (item) =>
              !applicationComponentIds.has(item.id) &&
              (item.runtime.application_key === "system" ||
                item.runtime.management_state === "observed"),
          )
          .map((item) => item.id),
      );
      return runtimeServiceGroups
        .map((group) => ({
          ...group,
          items: group.items.filter((item) => systemIDs.has(item.id)),
        }))
        .filter((group) => group.items.length > 0);
    })(),
  );

  const applicationSummary = $derived(() => {
    const applications = serviceApplications.filter((item) => !item.system);
    const statusOf = (item: ServiceApplication) => item.status.toLowerCase();
    return {
      total: applications.length,
      running: applications.filter((item) =>
        RUNNING_STATES.includes(statusOf(item)),
      ).length,
      pending: applications.filter((item) =>
        PENDING_STATES.includes(statusOf(item)),
      ).length,
      observed: applications.filter((item) =>
        ["observed", "mixed"].includes(applicationOwnership(item)),
      ).length,
      error: applications.filter((item) =>
        ERROR_STATES.includes(statusOf(item)),
      ).length,
    };
  });

  // Kept only for the Placement Board's independently loaded registry
  // projection.  The Applications tab below deliberately renders the
  // canonical runtime list, not these management rows.
  const summary = $derived(() => {
    const running = registry.services.filter(
      (service) => service.status === "running" || service.status === "healthy",
    ).length;
    const observed = registry.services.filter(
      (service) => serviceManagementState(service) === "observed",
    ).length;
    const pending = registry.services.filter((service) =>
      ["pending", "adopting", "starting"].includes(service.status),
    ).length;
    const error = registry.services.filter((service) =>
      ["error", "unhealthy"].includes(service.status),
    ).length;
    return {
      running,
      observed,
      pending,
      error,
      total: registry.services.length,
    };
  });

  const filteredServices = $derived(
    statusFilter === "all"
      ? registry.services
      : statusFilter === "observed"
        ? registry.services.filter(
            (service) => serviceManagementState(service) === "observed",
          )
        : statusFilter === "running"
          ? registry.services.filter((service) =>
              ["running", "healthy"].includes(service.status),
            )
          : registry.services.filter(
              (service) => service.status === statusFilter,
            ),
  );

  const servicesByServer = $derived(() =>
    registry.servers.map((server) => {
      const services = filteredServices.filter(
        (service) => service.server_id === server.id,
      );
      return {
        server,
        managed: services.filter(
          (service) => serviceManagementState(service) === "managed",
        ),
        unmanaged: services.filter(
          (service) => serviceManagementState(service) === "observed",
        ),
      };
    }),
  );

  $effect(() => {
    const selectedServer = registry.servers.find(
      (server) => server.id === selectedServerId,
    );
    const targetServer =
      selectedServer ??
      registry.servers.find(
        (server) => server.kit_deployment_id === selectedKitDeploymentId,
      ) ??
      registry.servers[0];
    selectedServerId = targetServer?.id ?? "";
    selectedKitDeploymentId =
      targetServer?.kit_deployment_id ?? registry.kit_deployments[0]?.id ?? "";
  });

  $effect(() => {
    if (
      selectedKitDeploymentId &&
      selectedKitDeploymentId !== cockpitState.stackId
    ) {
      void loadCockpit(selectedKitDeploymentId);
    }
  });

  async function loadRegistryManagement() {
    loading = true;
    registryError = null;
    try {
      registry = await listServiceRegistry();
    } catch (err) {
      registryError =
        err instanceof Error
          ? err.message
          : tr("ui.services.serviceRegistryCouldNotBe");
    } finally {
      loading = false;
    }
  }

  async function loadInventory() {
    inventoryLoading = true;
    inventoryError = null;
    try {
      const [services, applications] = await Promise.all([
        listCanonicalServicesReport(),
        listServiceApplicationsReport(),
      ]);
      canonicalServices = services.items;
      serviceApplications = applications.items;
      inventoryRejected = [...services.rejected, ...applications.rejected];
    } catch (err) {
      inventoryError =
        err instanceof Error
          ? err.message
          : tr("ui.services.runtimeInventoryCouldNotBe");
    } finally {
      inventoryLoading = false;
    }
  }

  async function loadServicesPage() {
    await Promise.allSettled([loadRegistryManagement(), loadInventory()]);
  }

  async function refreshRegistryQuietly() {
    const registryRefresh = listServiceRegistry()
      .then(async (nextRegistry) => {
        if (!servicesPageActive) return;
        registry = nextRegistry;
        if (selectedKitDeploymentId) {
          await loadCockpit(selectedKitDeploymentId);
        }
      })
      .catch(() => undefined);
    const inventoryRefresh = Promise.all([
      listCanonicalServicesReport(),
      listServiceApplicationsReport(),
    ])
      .then(([nextServices, nextApplications]) => {
        if (!servicesPageActive) return;
        canonicalServices = nextServices.items;
        serviceApplications = nextApplications.items;
        inventoryRejected = [
          ...nextServices.rejected,
          ...nextApplications.rejected,
        ];
      })
      .catch(() => undefined);
    await Promise.allSettled([registryRefresh, inventoryRefresh]);
    // Keep each last-known-good surface independently; the next manual load can
    // surface an error without letting a legacy registry failure hide Inventory.
  }

  async function loadCockpit(stackId: string) {
    const started = beginCockpitRefresh(cockpitState, stackId);
    cockpitState = started.state;
    try {
      const nextCockpit = await getMonitoringCockpit(stackId);
      const activeServerIDs = new Set(
        registry.servers
          .filter((server) => server.kit_deployment_id === stackId)
          .map((server) => server.id),
      );
      cockpitState = applyCockpitRefreshSuccess(
        cockpitState,
        started.request,
        nextCockpit,
        activeServerIDs,
      );
    } catch (err) {
      cockpitState = applyCockpitRefreshError(
        cockpitState,
        started.request,
        err,
      );
    }
  }

  function getServerCapacity(server: RegistryServer): ServerCapacity {
    return serverCapacity(
      cockpitServerFor(activeCockpit?.servers ?? [], server),
    );
  }

  function applicationLabel(service: RegistryService | null): string {
    return (
      service?.application_name ||
      service?.display_name ||
      tr("ui.services.application")
    );
  }

  function serviceDeployment(service: RegistryService) {
    return registry.kit_deployments.find(
      (deployment) => deployment.id === service.kit_deployment_id,
    );
  }

  function servicePlacement(service: RegistryService): ServicePlacement {
    const deployment = serviceDeployment(service);
    return resolveServiceCardPlacement(
      {
        ...service,
        provider:
          deployment?.provider_id ||
          deployment?.lease_provider ||
          deployment?.runtime_lane,
      },
      [
        deployment?.server_provisioning_mode,
        deployment?.runtime_offering_id,
        deployment?.provider_id,
        deployment?.lease_provider,
      ]
        .filter(Boolean)
        .join(" "),
    );
  }

  function serviceCardStatus(service: RegistryService): ServiceStatusKind {
    return resolveServiceCardStatus({
      ...service,
      operation_state: isServiceMigrating(service.id)
        ? service.migration_status || "queued"
        : undefined,
    });
  }

  function serviceMigration(
    service: RegistryService,
  ): ServiceMigration | undefined {
    if (!isServiceMigrating(service.id)) {
      return undefined;
    }
    const job = boardMoveFor(service.id)?.job;
    return {
      to: servicePlacement(service),
      progress:
        typeof job?.progress === "number"
          ? job.progress
          : migrationProgress(service.migration_status || "queued"),
      message: getMigrationMessage(service.id),
    };
  }

  function openService(service: RegistryService) {
    const url = service.url;
    if (!url || !serviceAccessEnabled(service)) return;
    window.open(url, "_blank", "noopener,noreferrer");
  }

  function serviceAccessEnabled(service: RegistryService): boolean {
    return serviceAccessEnabledFor(service, isServiceMigrating(service.id));
  }

  /** The backend offers a move only where the relocation API can make it. */
  function canMoveApplication(service: RegistryService): boolean {
    return (
      Boolean(service.move?.destinations.length) &&
      !isServiceMigrating(service.id) &&
      moveTargetsFor(service).length > 0
    );
  }

  function moveTargetsFor(service: RegistryService): RegistryServer[] {
    return moveTargetsAmong(registry.servers, service);
  }

  function moveTargetLabel(server: RegistryServer): string {
    const stack = registry.kit_deployments.find(
      (deployment) => deployment.id === server.kit_deployment_id,
    );
    return tr("applicationMove.serverOption", {
      server: server.name,
      stack: stack?.name || server.kit_deployment_id,
    });
  }

  function openMigration(service: RegistryService, target: RegistryServer) {
    if (!canMoveApplication(service)) return;
    moveSource = service;
    moveTarget = target;
    moveFailure = null;
    actionError = null;
  }

  const placementColumns = $derived.by((): PlacementColumn[] =>
    registry.servers.map((server) => {
      const deployment = registry.kit_deployments.find(
        (candidate) => candidate.id === server.kit_deployment_id,
      );
      return {
        server,
        cloud: Boolean(
          deployment?.server_mode === "cloud" ||
            deployment?.provider_id ||
            deployment?.lease_provider,
        ),
        capacity: getServerCapacity(server),
        cards: registry.services
          .filter((service) => service.server_id === server.id)
          .map((service) => {
            const canMove = canMoveApplication(service);
            return {
              key: service.id || `${service.kit_deployment_id}-${service.name}`,
              service,
              name: applicationLabel(service),
              meta: boardMeta(service),
              placement: servicePlacement(service),
              status: serviceCardStatus(service),
              migration: serviceMigration(service),
              canMove,
              moveTargetIds: canMove
                ? moveTargetsFor(service).map((target) => target.id)
                : [],
              ineligibleByServerId: ineligibleDestinationsByServer(service),
              migrating: isServiceMigrating(service.id),
              migrationMessage: getMigrationMessage(service.id),
              actions: [
                ...(serviceAccessEnabled(service)
                  ? [
                      {
                        id: "open" as const,
                        onSelect: () => openService(service),
                      },
                    ]
                  : []),
                {
                  id: "details" as const,
                  onSelect: () => openServiceSheet(service.id!),
                },
              ],
            };
          }),
        unmanaged: unmanagedCardsOn(server.id),
      };
    }),
  );

  const featureFlags = features.allFeatures;
  /** Default off. A missing flag definition keeps today's server columns. */
  const placementClassesOn = $derived(
    $featureFlags.get(PLACEMENT_BOARD_CLASSES_FLAG)?.enabled ??
      getDefaultEnabled(PLACEMENT_BOARD_CLASSES_FLAG),
  );

  function placementClassInput(runtime: CanonicalService) {
    return {
      id: runtime.id,
      name: runtime.application_display_name || runtime.name,
      applicationKey: runtime.application_key,
      serviceKey: runtime.service_key,
      catalogId: runtime.catalog?.id,
      targetKind: runtime.target_kind,
      placementClass: runtime.placement_class,
      serverId: runtime.server_id,
      providerId: runtime.placement?.provider_id,
    };
  }

  const placementClassView = $derived.by(() => {
    if (!placementClassesOn) return null;
    const partition = partitionPlacementBoard({
      applications: serviceApplications.map((application) => ({
        id: application.application_id,
        name: application.display_name,
        applicationKey: application.application_key,
        catalogId: application.catalog?.id,
        serverId: application.server_id,
        system: application.system,
        components: application.components.map(placementClassInput),
      })),
      services: canonicalServices.map(placementClassInput),
    });
    const lifted = new Set(partition.liftedIds);
    const cardFor = (
      item: (typeof partition.native)[number],
      native: boolean,
    ) => {
      const row = item.serviceId
        ? runtimeServiceRows.find(
            (candidate) => candidate.id === item.serviceId,
          )
        : undefined;
      const status = row?.status ?? "unknown";
      return {
        key: `${item.kind}:${item.id}`,
        name: item.name || row?.name || item.id,
        meta: native
          ? tr("ui.placementClasses.native.meta")
          : item.providerId
            ? tr("ui.placementClasses.managed.provider", {
                provider: item.providerId,
              })
            : tr("ui.placementClasses.managed.meta"),
        serviceId: item.serviceId,
        status,
        statusLabel: row?.statusLabel,
        native,
        managedServiceSlots: item.managedServiceSlots,
      };
    };
    return {
      native: partition.native.map((item) => cardFor(item, true)),
      managed: partition.managed.map((item) => cardFor(item, false)),
      columns: columnsWithoutLifted(placementColumns, lifted),
    };
  });

  /** Card meta on the board: a localized type and the port. */
  function boardMeta(service: RegistryService): string {
    const meta = serviceMeta(service);
    return service.type === "application"
      ? meta.replace(/^application/, tr("ui.services.application"))
      : meta;
  }

  /** Unmanaged containers of one Node, muted under its stacked applications. */
  function unmanagedCardsOn(serverId: string): UnmanagedCard[] {
    const ids = new Set(
      unmanagedOn(canonicalServices, serverId).map((service) => service.id),
    );
    return runtimeServiceRows
      .filter((row) => ids.has(row.runtime.id))
      .map((row) => {
        const stackState = row.runtime.adoption?.stack_it?.state;
        return {
          key: row.runtime.id,
          runtime: row.runtime,
          name: row.name,
          // The column is the server: no "discovered on" line.
          meta: "",
          placement: row.placement,
          status: row.status,
          canStack: row.runtime.allowed_actions.includes("adopt"),
          stacked: stackState === "stacked",
          actions: row.actions ?? [],
        };
      });
  }

  function cancelMigration() {
    if (moveSubmitting) return;
    moveSource = null;
    moveTarget = null;
    moveFailure = null;
  }

  /**
   * The owner's confirmation in the dialog is the approval the relocation API
   * requires. A retry of the same move reuses its Idempotency-Key, so a lost
   * acceptance never starts a second transfer.
   */
  async function confirmMigration(target: RegistryServer) {
    const service = moveSource;
    const move = service?.move;
    const destination = move?.destinations.find(
      (candidate) =>
        candidate.server_id === target.id &&
        candidate.kit_deployment_id === target.kit_deployment_id,
    );
    if (!service || !move || !destination || moveSubmitting) return;
    const stackId = service.kit_deployment_id;
    const request: ApplicationMoveRequest = {
      engine: "native",
      source_agent_id: move.source_agent_id,
      destination_stack_id: destination.kit_deployment_id,
      destination_agent_id: destination.agent_id,
      workloads: [move.workload_ref],
      owner_approved: true,
    };
    const keyScope = `move:${stackId}:workload:${move.workload_ref}`;
    const idempotencyKey = mintExtendWizardIdempotencyKey(
      keyScope,
      JSON.stringify(request),
    );
    moveSubmitting = true;
    moveFailure = null;
    try {
      const accepted = await startApplicationMove({
        sourceStackId: stackId,
        request,
        idempotencyKey,
      });
      clearExtendWizardIdempotencyKey(keyScope);
      const serviceIds = registry.services
        .filter(
          (candidate) =>
            candidate.kit_deployment_id === stackId &&
            candidate.server_id === service.server_id &&
            candidate.move?.workload_ref === move.workload_ref,
        )
        .map((candidate) => candidate.id)
        .filter((id): id is string => Boolean(id));
      boardMoves = {
        ...boardMoves,
        [`${stackId}\n${move.workload_ref}`]: {
          stackId,
          jobId: accepted.job_id,
          serviceIds,
          statusUnavailable: false,
        },
      };
      moveSource = null;
      moveTarget = null;
      void pollBoardMoves();
    } catch (cause) {
      moveFailure = parseApiError(cause).message;
    } finally {
      moveSubmitting = false;
    }
  }

  function isServiceMigrating(serviceId: string | undefined): boolean {
    if (!serviceId) return false;
    const service = registry.services.find((s) => s.id === serviceId);
    return hasTrackedMigration(serviceId) || hasPersistedMigration(service);
  }

  function hasActiveMigrations(): boolean {
    return (
      Object.values(boardMoves).some((move) => !moveJobTerminal(move.job)) ||
      registry.services.some((service) => hasPersistedMigration(service))
    );
  }

  function moveJobTerminal(job: JobDetail | undefined): boolean {
    return [
      "completed",
      "success",
      "failed",
      "error",
      "canceled",
      "cancelled",
    ].includes(job?.state ?? "");
  }

  async function pollBoardMoves() {
    // Only the polled keys are written back: a move accepted while this poll
    // awaits is kept as boardMoves holds it then.
    const next: Record<string, BoardMove> = {};
    let finished = false;
    for (const [key, move] of Object.entries(boardMoves)) {
      if (moveJobTerminal(move.job)) continue;
      try {
        const job = await getJob(move.jobId);
        // The move job belongs to the source StackKit.
        const foreign = Boolean(
          job.kit_deployment_id && job.kit_deployment_id !== move.stackId,
        );
        next[key] = foreign
          ? { ...move, statusUnavailable: true }
          : { ...move, job, statusUnavailable: false };
        finished ||= !foreign && moveJobTerminal(job);
      } catch {
        next[key] = { ...move, statusUnavailable: true };
      }
    }
    boardMoves = { ...boardMoves, ...next };
    if (finished) void refreshRegistryQuietly();
  }

  function getMigrationMessage(serviceId: string | undefined): string {
    if (!serviceId) return "";
    const move = boardMoveFor(serviceId);
    if (move && moveJobTerminal(move.job)) {
      const outcome = applicationMoveOutcome(move.job);
      return tr(`applicationMove.outcome.${outcome?.state ?? "failed"}`);
    }
    if (move?.statusUnavailable) return tr("applicationMove.statusUnavailable");
    if (move || isServiceMigrating(serviceId)) {
      return tr("applicationMove.running");
    }
    return "";
  }

  async function handleRemoveOld(serviceId: string) {
    if (
      !(await confirmInApp({
        title: tr("ui.services.deleteArchivedService"),
        message: tr("ui.services.thisPermanentlyDeletesTheArchived"),
        confirmText: tr("ui.settings.delete"),
        tone: "danger",
      }))
    ) {
      return;
    }
    try {
      saving = true;
      actionError = null;
      await deleteRegistryService(serviceId);
      registry = {
        ...registry,
        services: registry.services.filter((s) => s.id !== serviceId),
      };
    } catch (err) {
      actionError =
        err instanceof Error
          ? err.message
          : tr("ui.services.failedToDeleteArchivedService");
    } finally {
      saving = false;
    }
  }
</script>

{#if stackIt}{#key `${stackIt.service.id}:${stackIt.mode}`}<StackItDialog
      service={stackIt.service}
      mode={stackIt.mode}
      onClose={closeStackIt}
      onComplete={refreshRegistryQuietly}
    />{/key}{/if}

{#if adoptionService}{#key `${adoptionService.id}:${releaseAdoption}`}<ServiceAdoptionDialog
      service={adoptionService}
      release={releaseAdoption}
      onClose={closeAdoptionDialog}
      onComplete={refreshRegistryQuietly}
    />{/key}{/if}

<div
  class="p-4 md:p-6 animate-in fade-in duration-300"
  data-testid="services-page"
>
  <!-- One view-switcher pattern across the app: a group of toggle buttons,
       like the server list and Apps. It selects which content the page
       shows, so it stays visible at every width. -->
  {#snippet modeSwitch()}
    <div data-testid="services-mode-switch">
      <SegmentedControl
        items={[
          { value: "services", label: tr("ui.services.applications") },
          { value: "servers", label: tr("ui.services.placementBoard") },
        ]}
        value={activeTab}
        label={tr("ui.services.servicesMode")}
        onchange={(next) => (activeTab = next as "services" | "servers")}
      />
    </div>
  {/snippet}

  {#if wide.current}
    <PageHeader title={tr("ui.services.services")}>
      {#snippet actions()}
        <div
          class="flex flex-wrap items-center gap-3 animate-in slide-in-from-right-4 duration-300"
        >
          {@render modeSwitch()}

        </div>
      {/snippet}
    </PageHeader>
  {:else}
    <!-- Phone head: the switch is one row that never wraps (it scrolls
         sideways if a language is long). -->
    <PageHeader title={tr("ui.services.services")} />
    <div class="-mx-1 -mt-1 mb-3 overflow-x-auto whitespace-nowrap p-1">
      {@render modeSwitch()}
    </div>
  {/if}

  {#if actionError}
    <ErrorCallout
      class="mb-4"
      testId="services-action-error"
      message={actionError}
    />
  {/if}

  {#if activeTab === "services"}
    <!-- Applications View -->
    {#if inventoryLoading}
      <LoadingNote
        class="mb-6"
        label={tr("ui.services.loadingCanonicalServiceInventory")}
        testid="inventory-services-loading"
      />
    {:else if inventoryError}
      <ErrorCallout
        class="mb-6"
        testId="inventory-services-error-panel"
        message={inventoryError}
      />
    {/if}

    {#if inventoryRejected.length > 0}
      <section
        class="mb-6 flex gap-3 rounded-lg border border-warning/30 bg-warning/10 p-4 text-sm text-warning"
        role="status"
        data-testid="inventory-rows-rejected"
      >
        <AlertTriangle class="mt-0.5 h-5 w-5 shrink-0" />
        <div class="min-w-0">
          <p class="font-semibold">
            {trn("ui.serviceCards.rowsRejected", inventoryRejected.length)}
          </p>
          <p class="mt-1 text-warning/80">
            {tr("ui.serviceCards.rowsRejectedHint")}
          </p>
        </div>
      </section>
    {/if}

    {#if !loading && !registryError}
      {@const s = applicationSummary()}
      <div
        class="-mx-1 mb-4 overflow-x-auto whitespace-nowrap p-1 animate-in fade-in duration-200"
        data-testid="services-status-filter"
      >
        <SegmentedControl
          items={[
            { value: "all", label: `${tr("ui.services.all")} ${s.total}` },
            { value: "running", label: `${tr("ui.services.running")} ${s.running}` },
            { value: "pending", label: `${tr("ui.services.pending")} ${s.pending}` },
            { value: "observed", label: `${tr("ui.services.observed")} ${s.observed}` },
            { value: "error", label: `${tr("ui.services.error")} ${s.error}` },
          ]}
          value={statusFilter}
          label={tr("ui.services.applications")}
          onchange={(next) => (statusFilter = next)}
        />
      </div>
    {/if}

    {#if !inventoryLoading && !inventoryError}
      <ServiceList
        groups={applicationGroups}
        title={tr("ui.services.applications")}
        countLabel={trn(
          "ui.services.applicationsShown",
          applicationSummary().total,
          {
            shown: applicationRows.length,
          },
        )}
        emptyTitle={serviceApplications.length === 0
          ? tr("ui.services.noApplicationsReported")
          : tr("ui.services.noApplicationsMatching", { status: statusFilter })}
        emptyBody={serviceApplications.length === 0
          ? tr("ui.services.applicationsAppearAfterStackkitsRuntime")
          : tr("ui.services.clearTheFilterToShow")}
        testId="runtime-service-list"
        cardTestId="runtime-service-card"
      >
        {#snippet children(item)}
          {@const application = applicationById.get(item.id)}
          {#if application}
            <!-- Actions live on the card's capability row; governed operations
                 (verify, move, adopt, delete) live in the detail sheet. The
                 inline strip keeps each component's last action receipt
                 visible (an application id is not a service id, so the jobs
                 are looked up per component). -->
            {#each application.components as component (component.id)}
              {@const serviceActionJob = tracker.jobs[component.id]}
              {#if serviceActionJob && (serviceActionJob.action !== "logs" || actionFailed(serviceActionJob))}
                <div
                  class="mt-1 rounded border border-border/70 bg-muted/25 p-2 text-xs"
                  data-testid="managed-service-action-status"
                  data-service-id={component.id}
                  role={actionFailed(serviceActionJob) ? "alert" : "status"}
                >
                  <p class="font-medium text-foreground">
                    {tr("ui.serviceCards.jobLine", {
                      name: component.name,
                      action: actionLabel(serviceActionJob.action),
                      state: label(serviceActionJob.state),
                    })}
                    {#if serviceActionJob.converging}{tr(
                        "ui.services.verifying",
                      )}{/if}
                  </p>
                  {#if serviceActionJob.detail}<p
                      class="mt-1 text-muted-foreground"
                    >
                      {serviceActionJob.detail}
                    </p>{/if}
                </div>
              {/if}
            {/each}
          {/if}
        {/snippet}
      </ServiceList>

      {#if systemServiceGroups.length > 0}
        <div class="mt-10">
          <ServiceList
            groups={systemServiceGroups}
            title={tr("ui.services.systemServices")}
            countLabel={tr(
              "ui.services.observedTechnicalComponentsWithoutApplication",
            )}
            emptyTitle={tr("ui.services.noSystemServices")}
            emptyBody={tr(
              "ui.services.unclassifiedRuntimeComponentsAppearHere",
            )}
            display="adaptive"
            testId="system-service-list"
            cardTestId="system-service-card"
          />
        </div>
      {/if}

      <TrademarkNotice class="mt-6" />
    {/if}

    {#if registryError}
      <div
        class="flex gap-3 rounded-lg border border-warning/30 bg-warning/10 p-4 text-sm text-warning animate-in fade-in"
        role="status"
        data-testid="registry-services-warning"
      >
        <AlertTriangle class="mt-0.5 h-5 w-5 shrink-0 text-warning" />
        <div class="min-w-0">
          <p class="font-semibold">
            {tr("ui.services.applicationManagementIsTemporarilyUnavailable")}
          </p>
          <p class="mt-1 text-warning/80">
            {tr("ui.services.anyRuntimeInventoryShownAbove")}
          </p>
          <p class="mt-2 break-words font-mono text-xs text-warning/70">
            {registryError}
          </p>
          <Button
            variant="secondary"
            size="sm"
            class="mt-3"
            onclick={() => void loadRegistryManagement()}
          >
            {tr("ui.services.retryManagementData")}
          </Button>
        </div>
      </div>
    {/if}
  {:else if placementClassView}
    <PlacementClassBoard
      native={placementClassView.native}
      managed={placementClassView.managed}
      columns={placementClassView.columns}
      {loading}
      {registryError}
      {cockpitStale}
      {cockpitError}
      onretry={() => void loadRegistryManagement()}
      onopen={(serviceId) => openServiceSheet(serviceId)}
      onmove={openMigration}
      onstack={(card) => openAdoptionDialog(card.runtime, false)}
    />
  {:else}
    <PlacementBoard
      columns={placementColumns}
      {loading}
      {registryError}
      {cockpitStale}
      {cockpitError}
      onretry={() => void loadRegistryManagement()}
      onmove={openMigration}
      onstack={(card) => openAdoptionDialog(card.runtime, false)}
    />
  {/if}
</div>

{#if moveSource && moveTarget}
  <ServiceMoveDialog
    applicationName={applicationLabel(moveSource)}
    sourceNodeName={moveSource.server_name ?? ""}
    targets={moveTargetsFor(moveSource).map((server) => ({
      server,
      label: moveTargetLabel(server),
    }))}
    initialTargetId={moveTarget.id}
    warnings={moveSource.move?.warnings ?? []}
    blockedTargets={blockedMoveTargetsAmong(registry.servers, moveSource).map(
      ({ server, entry }) => ({
        server,
        label: moveTargetLabel(server),
        code: entry.code,
      }),
    )}
    submitting={moveSubmitting}
    failure={moveFailure}
    oncancel={cancelMigration}
    onconfirm={confirmMigration}
  />
{/if}


{#if detailOpen}
  {@const detailRuntime = detailRow?.runtime}
  {@const detailName =
    detailRow?.name ||
    (detailRegistryService
      ? applicationLabel(detailRegistryService)
      : tr("ui.services.service"))}
  <ServiceDetailSheet
    open={detailOpen}
    onclose={closeServiceSheet}
    name={detailName}
    meta={detailRow?.meta ||
      (detailRegistryService ? serviceMeta(detailRegistryService) : undefined)}
    status={detailRow?.status ||
      (detailRegistryService
        ? serviceCardStatus(detailRegistryService)
        : "unknown")}
    statusLabel={detailRow?.statusLabel}
    locked={serviceIsLocked(detailRuntimeService?.mutation_lock)}
    lockedReason={serviceLockReason(detailRuntimeService?.mutation_lock)}
    applicationHref={detailApplication
      ? `/services/${encodeURIComponent(detailApplication.application_id)}`
      : undefined}
    applicationLabel={detailApplication
      ? `${detailApplication.display_name} · ${trn("ui.services.componentCount", detailApplication.components.length)}`
      : undefined}
    bind:tab={detailTab}
  >
    {#snippet overview()}
      <div class="flex flex-col gap-4" data-testid="service-sheet-overview">
        {#if detailRuntime}
          <div class="grid grid-cols-3 gap-2">
            <div class="rounded-lg bg-muted/30 px-2.5 py-2">
              <div
                class="text-[9px] uppercase tracking-wider text-muted-foreground"
              >
                {tr("ui.services.desired")}
              </div>
              <div class="mt-0.5 font-mono text-[12.5px] font-semibold">
                {stateLabel(detailRuntime.desired_state)}
              </div>
            </div>
            <div class="rounded-lg bg-muted/30 px-2.5 py-2">
              <div
                class="text-[9px] uppercase tracking-wider text-muted-foreground"
              >
                {tr("ui.services.observed")}
              </div>
              <div class="mt-0.5 font-mono text-[12.5px] font-semibold">
                {stateLabel(detailRuntime.observed_state)}
              </div>
            </div>
            <div class="rounded-lg bg-muted/30 px-2.5 py-2">
              <div
                class="text-[9px] uppercase tracking-wider text-muted-foreground"
              >
                {tr("ui.services.health")}
              </div>
              <div class="mt-0.5 font-mono text-[12.5px] font-semibold">
                {stateLabel(detailRuntime.health.state)}
              </div>
            </div>
          </div>
          <div
            class="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground"
          >
            <span
              >{tr("ui.serviceSheet.ownership", {
                value: detailRuntime.management_state,
              })}</span
            >
            <span
              >{tr("ui.services.targetValue", {
                value:
                  detailRow?.targetLabel || detailRow?.runtimeTargetId || "",
              })}</span
            >
            <span
              >{tr("ui.services.freshnessValue", {
                value: detailRow?.freshnessLabel ?? "",
              })}</span
            >
            {#if detailRow?.sourceLabel}<span
                >{tr("ui.common.sourceValue", {
                  value: detailRow.sourceLabel,
                })}</span
              >{/if}
          </div>
          {#if typeof detailRuntime.access.url === "string" && detailRuntime.access.url}
            <div
              class="flex items-center gap-2 rounded-lg bg-muted/30 px-2.5 py-2"
            >
              <span class="font-mono text-xs flex-1 truncate"
                >{detailRuntime.access.url}</span
              >
              <Button
                variant="secondary"
                size="sm"
                onclick={() =>
                  openServiceUrl({ url: String(detailRuntime.access.url) })}
                >{tr("ui.services.open")}</Button
              >
            </div>
          {/if}
        {:else if detailRegistryService}
          <p class="text-sm text-muted-foreground">
            {tr("ui.services.noCanonicalRuntimeObservationYet")}
          </p>
          <div
            class="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground"
          >
            <span
              >{tr("ui.services.statusValue", {
                value: detailRegistryService.status || tr("ui.common.unknown"),
              })}</span
            >
            {#if detailRegistryService.server_name}<span
                >{tr("ui.serviceSheet.node", {
                  node: detailRegistryService.server_name,
                })}</span
              >{/if}
          </div>
        {/if}
        {#if detailApplication && detailApplication.components.length > 0}
          <div>
            <div
              class="mb-2 text-[9px] uppercase tracking-wider text-muted-foreground"
            >
              {tr("ui.services.components")}
            </div>
            <div
              class="flex flex-col gap-1.5"
              data-testid="service-sheet-components"
            >
              {#each detailApplication.components as component (component.id)}
                {@const componentHealthy = ["healthy", "running"].includes(
                  (
                    component.health.state ||
                    component.observed_state ||
                    ""
                  ).toLowerCase(),
                )}
                <div
                  class="flex items-center gap-2.5 rounded-lg bg-muted/30 px-2.5 py-2"
                >
                  <span
                    class="h-2 w-2 rounded-full {componentHealthy
                      ? 'bg-success'
                      : 'bg-warning'}"
                  ></span>
                  <span class="flex-1 truncate font-mono text-[11.5px]">
                    {component.name || component.service_key}
                  </span>
                  <span class="font-mono text-[10px] text-muted-foreground">
                    {stateLabel(component.observed_state)} · {stateLabel(
                      component.health.state,
                    )}
                  </span>
                </div>
              {/each}
            </div>
          </div>
        {/if}
      </div>
    {/snippet}
    {#snippet activity()}
      <div class="flex flex-col gap-3" data-testid="service-sheet-activity">
        {#if detailJob}
          <div
            class="rounded-lg border border-border/70 bg-muted/25 p-3 text-xs"
            role={actionFailed(detailJob) ? "alert" : "status"}
          >
            <p class="font-medium text-foreground">
              {actionLabel(detailJob.action)}: {label(detailJob.state)}
              {#if detailJob.converging}{tr("ui.services.verifying")}{/if}
            </p>
            {#if detailJob.jobId}<p
                class="mt-1 font-mono text-muted-foreground"
              >
                {tr("ui.services.jobId", { id: detailJob.jobId })}
              </p>{/if}
            {#if detailJob.detail}<p class="mt-1 text-muted-foreground">
                {detailJob.detail}
              </p>{/if}
          </div>
        {:else}
          <p class="text-sm text-muted-foreground">
            {tr("ui.services.noGovernedActionHasRun")}
          </p>
        {/if}
        {#if detailRow?.workflowLabel}
          <p class="text-xs text-muted-foreground">
            {tr("ui.services.operationValue", {
              value: detailRow.workflowLabel,
            })}
          </p>
        {/if}
      </div>
    {/snippet}
    {#snippet logs()}
      <div class="flex flex-col gap-3" data-testid="service-sheet-logs">
        {#if detailRuntime}
          <div class="flex items-center gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={isActionActive(detailJob)}
              onclick={() => void requestServiceAction(detailRuntime, "logs")}
            >
              {detailLogState
                ? tr("ui.services.refreshLogs")
                : tr("ui.services.fetchLogs")}
            </Button>
            {#if detailLogState?.loading}<Loader2
                class="h-3.5 w-3.5 animate-spin text-muted-foreground"
              />{/if}
          </div>
        {/if}
        {#if detailLogState?.error}
          <ErrorCallout class="p-2 text-xs" message={detailLogState.error} />
        {/if}
        {#if detailLogState && detailLogState.entries.length > 0}
          <ol class="max-h-72 space-y-1 overflow-auto font-mono text-[11px]">
            {#each detailLogState.entries as entry (`${entry.timestamp}-${entry.message}`)}
              <li class="rounded bg-background/60 p-1.5">
                <time class="text-muted-foreground" datetime={entry.timestamp}
                  >{entry.timestamp}</time
                >
                <pre
                  class="whitespace-pre-wrap break-words text-foreground">{entry.message}</pre>
              </li>
            {/each}
          </ol>
          {#if detailLogState.nextCursor && detailRuntime}
            <Button
              variant="secondary"
              size="sm"
              disabled={isActionActive(detailJob)}
              onclick={() => tracker.loadMoreLogs(detailRuntime)}
              >{tr("ui.services.loadOlderLogs")}</Button
            >
          {/if}
        {:else if detailLogState && !detailLogState.loading}
          <p class="text-sm text-muted-foreground">
            {tr("ui.services.theLastLogPageWas")}
          </p>
        {/if}
      </div>
    {/snippet}
    {#snippet operations()}
      <div class="flex flex-col gap-2" data-testid="service-sheet-operations">
        {#if detailRuntime && detailStackIt?.state === "stacked"}
          <div
            class="rounded-lg bg-muted/30 px-2.5 py-2 text-xs"
            data-testid="stacked-original"
          >
            {detailStackIt.retain_until
              ? tr("ui.services.originalKeptUntil", {
                  date: formatDate(detailStackIt.retain_until),
                })
              : tr("ui.services.originalKept")}
          </div>
          <div class="flex flex-wrap gap-2">
            <Button
              variant="secondary"
              size="sm"
              disabled={!detailStackIt.restore_available}
              onclick={() =>
                openStackedOriginal(detailRuntime, "restore_original")}
              testId="restore-original"
            >
              {tr("ui.services.restoreOriginal")}
            </Button>
            <Button
              variant="destructive"
              size="sm"
              disabled={!detailStackIt.release_original_available}
              onclick={() =>
                openStackedOriginal(detailRuntime, "release_original")}
              testId="remove-original"
            >
              {tr("ui.services.removeOriginal")}
            </Button>
          </div>
        {:else if detailRuntime?.management_state === "observed"}
          {@const stackable = Boolean(
            detailRuntime.adoption?.preview_available ||
              detailRuntime.adoption?.stack_it?.available,
          )}
          <Button
            variant="secondary"
            size="sm"
            onclick={() => openAdoptionDialog(detailRuntime, false)}
            disabled={!stackable}
            testId="stack-it-service"
          >
            {tr("ui.services.stackIt")}
          </Button>
          {#if !stackable}<p
              class="text-sm text-muted-foreground"
            >
              {adoptionUnavailableReason(detailRuntime.adoption)}
            </p>{/if}
        {/if}
        {#if detailRuntime?.adoption?.release_available}<Button
            variant="secondary"
            onclick={() => openAdoptionDialog(detailRuntime, true)}
            >{tr("ui.services.releaseBinding")}</Button
          >{/if}
        {#if detailRegistryService?.status === "archived"}
          <Button
            variant="destructive"
            size="sm"
            onclick={() => handleRemoveOld(detailRegistryService.id!)}
            disabled={saving}
          >
            <Trash2 class="h-3.5 w-3.5" />
            {tr("ui.services.removeOldCopy")}
          </Button>
        {/if}
        {#if !detailRegistryService && !detailRuntime}
          <p class="text-sm text-muted-foreground">
            {tr("ui.services.noOperationsAvailable")}
          </p>
        {/if}
        <p class="mt-1 text-[10.5px] text-muted-foreground">
          {tr("ui.services.mutationsRequireOwnerApprovalA")}
        </p>
      </div>
    {/snippet}
  </ServiceDetailSheet>
{/if}
