/**
 * Three hosting classes for the placement board (D-2026-11-21-02 Phase 3C).
 *
 * The helper reads placement fields the services API already returns
 * (`target_kind`: "server" | "managed_workload" | "unknown", plus catalog
 * identity). It does not invent a host, a route or a slot meter.
 *
 * 1. Native kombify apps — hosted Paperwork, kombify Sites and the
 *    Mediacenter cloud folder. They consume 0 managed-service slots.
 *    A copy whose `target_kind` is "server", or that already sits on a
 *    server without a managed-workload placement, stays on that server
 *    (self-hosted Paperwork is not the hosted product).
 * 2. Managed Apps — `target_kind === "managed_workload"`. They belong in
 *    one Managed Area, never in a fabricated server column.
 * 3. Dedicated servers — self-hosted workloads and dedicated managed
 *    servers. The existing server columns draw this class.
 */
import type { PlacementColumn } from "./placement.js";

export type PlacementHostingClass =
  | "native"
  | "managed_app"
  | "dedicated_server"
  | "unclassified";

/** Identity and placement facts already present on a canonical service row. */
export interface PlacementClassInput {
  id: string;
  name?: string;
  applicationKey?: string;
  serviceKey?: string;
  catalogId?: string;
  targetKind?: string;
  placementClass?: string;
  serverId?: string;
  providerId?: string;
}

export interface PlacementClassApplication {
  id: string;
  name?: string;
  applicationKey?: string;
  catalogId?: string;
  serverId?: string;
  system?: boolean;
  components: readonly PlacementClassInput[];
}

/** One card in the native area or the Managed Area. */
export interface PlacementAreaItem {
  kind: "application" | "service";
  id: string;
  name?: string;
  /** Existing service sheet id. The first component, for an application. */
  serviceId?: string;
  providerId?: string;
  /** Native cards always report 0. Other classes do not invent a count. */
  managedServiceSlots: 0 | null;
}

export interface PlacementPartition {
  native: PlacementAreaItem[];
  managed: PlacementAreaItem[];
  /** Canonical ids removed from dedicated server columns so they are not drawn twice. */
  liftedIds: string[];
}

/**
 * Hosted products named by D-2026-11-21-02. Matched on the whole machine token
 * (catalog id, application key or service key), never display name or substring.
 */
const NATIVE_APP_KEYS = new Set([
  "paperwork",
  "kombify-paperwork",
  "kombify-sites",
  "kombify-site",
  "sites",
  "mediacenter",
  "media-center",
  "kombify-mediacenter",
  "mediacenter-cloud",
  "mediacenter-cloud-folder",
]);

function token(value: string | undefined): string {
  return (value || "")
    .trim()
    .toLowerCase()
    .replace(/[\s_]+/g, "-");
}

export function isNativeKombifyApp(input: PlacementClassInput): boolean {
  return [
    input.applicationKey,
    input.serviceKey,
    input.catalogId,
  ].some((candidate) => NATIVE_APP_KEYS.has(token(candidate)));
}

export function classifyPlacement(
  input: PlacementClassInput,
): PlacementHostingClass {
  const targetKind = (input.targetKind || "").trim();
  const placementClass = (input.placementClass || "").trim().toLowerCase();
  const onServer = Boolean(input.serverId);
  const native = isNativeKombifyApp(input);
  if (targetKind === "server") return "dedicated_server";
  if (targetKind === "managed_workload") {
    return native ? "native" : "managed_app";
  }
  if (native && !onServer && placementClass === "managed") {
    return "native";
  }
  if (onServer) return "dedicated_server";
  return "unclassified";
}

function areaItem(
  kind: PlacementAreaItem["kind"],
  id: string,
  name: string | undefined,
  serviceId: string | undefined,
  providerId: string | undefined,
  hostingClass: "native" | "managed_app",
): PlacementAreaItem {
  return {
    kind,
    id,
    name,
    serviceId,
    providerId,
    managedServiceSlots: hostingClass === "native" ? 0 : null,
  };
}

function pushArea(
  partition: PlacementPartition,
  hostingClass: "native" | "managed_app",
  item: PlacementAreaItem,
): void {
  (hostingClass === "native" ? partition.native : partition.managed).push(item);
}

/**
 * Collapse an application into one native or managed card when every
 * component agrees. Mixed applications keep dedicated components on their
 * server and lift only the native or managed components.
 * System applications stay out of both areas.
 */
export function partitionPlacementBoard(args: {
  applications: readonly PlacementClassApplication[];
  services: readonly PlacementClassInput[];
}): PlacementPartition {
  const claimed = new Set<string>();
  const partition: PlacementPartition = {
    native: [],
    managed: [],
    liftedIds: [],
  };

  for (const application of args.applications) {
    if (application.system) {
      // Platform components stay on their server column. They are not Managed Apps.
      for (const component of application.components) claimed.add(component.id);
      continue;
    }
    const components = application.components;
    if (components.length === 0) continue;
    const decisions = components.map((component) =>
      classifyPlacement(component),
    );
    const classes = new Set(decisions);
    const shared = classes.size === 1 ? decisions[0] : undefined;
    if (shared === "native" || shared === "managed_app") {
      pushArea(
        partition,
        shared,
        areaItem(
          "application",
          application.id,
          application.name,
          components[0]?.id,
          components.find((component) => component.providerId)?.providerId,
          shared,
        ),
      );
      for (const component of components) {
        claimed.add(component.id);
        partition.liftedIds.push(component.id);
      }
      continue;
    }
    components.forEach((component, index) => {
      const decision = decisions[index];
      if (decision !== "native" && decision !== "managed_app") return;
      claimed.add(component.id);
      partition.liftedIds.push(component.id);
      pushArea(
        partition,
        decision,
        areaItem(
          "service",
          component.id,
          component.name,
          component.id,
          component.providerId,
          decision,
        ),
      );
    });
  }

  for (const service of args.services) {
    if (claimed.has(service.id)) continue;
    const decision = classifyPlacement(service);
    if (decision !== "native" && decision !== "managed_app") continue;
    partition.liftedIds.push(service.id);
    pushArea(
      partition,
      decision,
      areaItem(
        "service",
        service.id,
        service.name,
        service.id,
        service.providerId,
        decision,
      ),
    );
  }

  const byName = (left: PlacementAreaItem, right: PlacementAreaItem) =>
    (left.name || left.id).localeCompare(right.name || right.id);
  partition.native.sort(byName);
  partition.managed.sort(byName);
  return partition;
}

/** Server columns with native and managed workloads removed. The server itself stays. */
export function columnsWithoutLifted(
  columns: readonly PlacementColumn[],
  liftedIds: ReadonlySet<string>,
): PlacementColumn[] {
  if (liftedIds.size === 0) return [...columns];
  return columns.map((column) => ({
    ...column,
    cards: column.cards.filter(
      (card) => !card.service.id || !liftedIds.has(card.service.id),
    ),
    unmanaged: column.unmanaged.filter(
      (card) => !liftedIds.has(card.runtime.id),
    ),
  }));
}
