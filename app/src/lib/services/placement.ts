import type { CanonicalService } from "#lib/api/services.js";
import type {
  RegistryCatalogService,
  RegistryServer,
  RegistryService,
} from "#lib/api/registry.js";
import type { StackOperationServer } from "#lib/api/stacks.js";
import type {
  ServiceCardAction,
  ServiceMigration,
  ServicePlacement,
  ServiceStatusKind,
} from "@kombiverselabs/ui/service";

/** Capacity of one Node for the Placement board, from the cockpit snapshot. */
export interface ServerCapacity {
  storageFree?: number;
  storageTotal?: number;
  storagePercent?: number;
  ramUsed?: number;
  ramTotal?: number;
  ramPercent?: number;
  source: "telemetry" | "capabilities" | "unknown";
}

export function cockpitServerFor(
  cockpitServers: StackOperationServer[],
  server: RegistryServer,
): StackOperationServer | undefined {
  return cockpitServers.find(
    (candidate) =>
      candidate.id === server.id ||
      candidate.id === server.worker_id ||
      candidate.hostname === server.hostname ||
      candidate.hostname === server.name,
  );
}

export function serverCapacity(
  opsServer: StackOperationServer | undefined,
): ServerCapacity {
  if (!opsServer) return { source: "unknown" };

  const diskPercent = opsServer.health?.disk_percent?.value;
  const ramPercent = opsServer.health?.memory_percent?.value;
  const diskTotal = Number(opsServer.capabilities?.disk_gb ?? 0);
  const ramTotalGb = Number(opsServer.capabilities?.ram_mb ?? 0) / 1024;

  if (diskTotal <= 0 && ramTotalGb <= 0) return { source: "unknown" };

  return {
    storageTotal: diskTotal > 0 ? Math.round(diskTotal) : undefined,
    storageFree:
      diskTotal > 0 && typeof diskPercent === "number"
        ? Number((diskTotal * (1 - diskPercent / 100)).toFixed(1))
        : undefined,
    storagePercent:
      typeof diskPercent === "number" ? Math.round(diskPercent) : undefined,
    ramTotal: ramTotalGb > 0 ? Number(ramTotalGb.toFixed(1)) : undefined,
    ramUsed:
      ramTotalGb > 0 && typeof ramPercent === "number"
        ? Number((ramTotalGb * (ramPercent / 100)).toFixed(1))
        : undefined,
    ramPercent:
      typeof ramPercent === "number" ? Math.round(ramPercent) : undefined,
    source:
      typeof diskPercent === "number" || typeof ramPercent === "number"
        ? "telemetry"
        : "capabilities",
  };
}

export function serviceTypeLabel(
  service: RegistryCatalogService | RegistryService,
): string {
  return service.type.replace(/[-_]/g, " ");
}

export function serviceMeta(service: RegistryService): string {
  const meta = [serviceTypeLabel(service)];
  if (service.port) meta.push(`:${service.port}`);
  return meta.join(" · ");
}

/** Exactly the servers the backend admits as destination of this move. */
export function moveTargetsFor(
  servers: RegistryServer[],
  service: RegistryService,
): RegistryServer[] {
  const destinations = service.move?.destinations ?? [];
  return servers.filter((server) =>
    destinations.some(
      (destination) =>
        destination.server_id === server.id &&
        destination.kit_deployment_id === server.kit_deployment_id,
    ),
  );
}

/** Statuses in which a service has no address a person can open. */
const CLOSED_STATUSES = [
  "unknown",
  "migrating",
  "deploying",
  "pending_verification",
  "archived",
  "pending",
  "adopting",
  "stopped",
  "offline",
];

export function serviceAccessEnabled(
  service: RegistryService,
  migrating: boolean,
): boolean {
  if (!service.url || migrating) return false;
  return !CLOSED_STATUSES.includes(service.status.toLowerCase());
}

/** Progress shown on a moving card, by the phase the backend reports. */
export function migrationProgress(phase: string | undefined): number {
  return phase === "pending_verification"
    ? 95
    : phase === "deploying"
      ? 75
      : 45;
}

/** One application card on the Placement board, resolved by the page. */
export interface PlacementCard {
  key: string;
  service: RegistryService;
  name: string;
  meta: string;
  placement: ServicePlacement;
  status: ServiceStatusKind;
  migration?: ServiceMigration;
  canMove: boolean;
  /** Server ids this card may be dropped on; empty when it cannot move. */
  moveTargetIds: string[];
  migrating: boolean;
  migrationMessage: string;
  actions: ServiceCardAction[];
}

/** One Node column on the Placement board. */
export interface PlacementColumn {
  server: RegistryServer;
  /** The Node is a cloud VPS rather than local hardware. */
  cloud: boolean;
  capacity: ServerCapacity;
  cards: PlacementCard[];
  /** Unmanaged containers observed on this Node. */
  unmanaged: UnmanagedCard[];
}

/**
 * One unmanaged container on its Node, running outside any stack. Dropping it
 * on the stacked area of its own Node opens "Stack it"; it moves once stacked.
 */
export interface UnmanagedCard {
  key: string;
  runtime: CanonicalService;
  name: string;
  meta: string;
  placement: ServicePlacement;
  status: ServiceStatusKind;
  /** The backend admits Stack it for this container now. */
  canStack: boolean;
  /** Already stacked; the stopped original is kept until its retention ends. */
  stacked: boolean;
  actions: ServiceCardAction[];
}

/** Unmanaged containers of one Node; StackKits-run own applications excluded. */
export function unmanagedOn(
  services: CanonicalService[],
  serverId: string,
): CanonicalService[] {
  return services.filter(
    (service) =>
      service.server_id === serverId &&
      service.management_state === "observed" &&
      !service.own_application &&
      service.adoption?.stack_it?.state !== "released",
  );
}
