import { isAppIconUrl } from "../brand-logo";
import { fetchApi } from "./client";
import { parseManagementState, type RegistryManagementState } from "./registry";

export type ManagedServiceAction =
  "start" | "stop" | "restart" | "logs" | "freeze" | "unfreeze" | "update" | "backup";

/**
 * Guardrail actions are applied synchronously by the control plane: no job is
 * enqueued and no agent is contacted, so they carry no inventory revision and
 * their response has no job id.
 */
export const SERVICE_LOCK_ACTIONS: ReadonlySet<ManagedServiceAction> = new Set([
  "freeze",
  "unfreeze",
]);

/**
 * Canonical service read model (`GET /api/v1/services`,
 * `GET /api/v1/services/{id}`), served from the persisted service aggregate
 * (migration 073/074, `ApplyServiceEvent`).
 */
export interface CanonicalServiceHealth {
  state: string;
  observed_at?: string;
  reason_code?: string;
}

export interface CanonicalPlacementFreshness {
  state: "recorded" | "unknown" | string;
  age_seconds?: number;
}

export interface CanonicalServicePlacement {
  provider_id?: string;
  managed_target_ref?: string;
  provider_receipt_ref?: string;
  sla_policy_ref?: string;
  backup_policy_ref?: string;
  evidence_ref?: string;
  observed_at?: string;
  freshness: CanonicalPlacementFreshness;
}

export interface CanonicalServiceMutationLock {
  state: "unlocked" | "locked" | string;
  reason_code?: string;
  actor?: string;
  changed_at?: string;
}

/** Catalog recognition of a service or application (additive backend field). */
export interface ServiceCatalogRef {
  id: string;
  display_name?: string;
  category?: string;
  /** Vendor domain used for the logo lookup. */
  brand_domain?: string;
  /** Default UI port inside the container. */
  web_port?: number;
  /** Our guide or article for this application, when one exists. */
  guide_url?: string;
  /** The upstream project, from the StackKits catalog (additive backend field). */
  vendor?: ServiceVendor;
  /** Official end-user clients (stores and installers), when the catalog lists any. */
  clients?: ServiceClient[];
  /** The project website of the recognized application (https only). */
  homepage_url?: string;
  /** How the application was recognized, strongest evidence first. */
  evidence?: ServiceCatalogEvidence;
  /**
   * The application's logo on our own asset host. Absent means no logo is
   * known (or it was removed): draw a monogram, never a third-party image.
   */
  logo?: ServiceCatalogLogo;
  /** The catalog removed this application's logo: draw the monogram. */
  logo_removed?: boolean;
}

/** The layer that recognized an application (container labels beat image names). */
export type ServiceCatalogEvidence =
  "label" | "image" | "name" | "http_fingerprint" | "released_catalog";

/** Where a mirrored logo comes from; every source is mirrored on our asset host. */
export type ServiceCatalogLogoSource =
  "dashboard-icons" | "simple-icons" | "kombify";

export interface ServiceCatalogLogo {
  /** The logo, an SVG when mirrored, otherwise a PNG. */
  url: string;
  /** Variant for a light UI theme, when the icon set ships one. */
  light_url?: string;
  /** Variant for a dark UI theme, when the icon set ships one. */
  dark_url?: string;
  source: ServiceCatalogLogoSource;
}

export interface ServiceSupport {
  kind: "donate" | "sponsor" | "purchase";
  url: string;
}

export interface ServiceVendor {
  name?: string;
  website?: string;
  docs?: string;
  license?: string;
  support?: ServiceSupport[];
}

export interface ServiceClient {
  platform: "android" | "ios" | "windows" | "macos" | "linux";
  kind: "store" | "download";
  url: string;
}

/**
 * Measured version facts (additive backend field). `current` and `latest`
 * are never invented: `latest` exists only after a real registry or release
 * check succeeded, otherwise `reason_code` says why it is unknown.
 */
export interface ServiceVersionInfo {
  current?: string;
  source?: "image_tag" | "image_label" | "stackkit" | "image_digest" | string;
  image?: string;
  latest?: string;
  update_available?: boolean;
  checked_at?: string;
  update_mode?:
    | "stackkit_upgrade"
    | "adopt_required"
    | "unavailable"
    | "application_update"
    | "bundled"
    | string;
  reason_code?: string;
  /**
   * Application release facts of a StackKits-managed application from the
   * verified StackKits application release feed. `not_verified` is neither
   * up to date nor free of known vulnerabilities.
   */
  evidence_status?: "verified" | "not_verified" | string;
  evidence_reason_code?: string;
  observed_at?: string;
  update_setting?: "ask" | "automatic" | "with_stackkits_releases" | string;
  latest_digest?: string;
  /** The offered version is checked (tested) by kombify. */
  tested?: boolean;
  release_notes_url?: string;
  newer_versions?: ApplicationVersionOffer[];
  advisories?: ApplicationAdvisory[];
  security_fix?: ApplicationVersionOffer & { advisories: string[] };
}

export interface ApplicationVersionOffer {
  version: string;
  digest?: string;
  tested: boolean;
  release_notes_url?: string;
  published_at?: string;
}

export interface ApplicationAdvisory {
  id: string;
  severity: "critical" | "high" | "medium" | "low" | "unknown" | string;
  cvss?: number;
  fixed_version?: string;
  url: string;
  /** `installed` when evaluated against the running version, else `unknown`. */
  affects: "installed" | "unknown" | string;
  high: boolean;
}

/** Where the backend says a service runs; `serverless` is not emitted. */
export type ServicePlacementClass = "local" | "cloud" | "managed" | "unknown";

export interface CanonicalService {
  adoption?: {
    preview_available: boolean;
    execution_available: boolean;
    release_available: boolean;
    profile_ref?: string;
    reason_code?: string;
    /** "Stack it": rebuild this unmanaged container as an own application. */
    stack_it?: StackItAvailability;
  };
  /** Present on a container StackKits runs as an own application. */
  own_application?: OwnApplicationProjection;
  id: string;
  kit_deployment_id: string;
  server_id?: string;
  target_kind: "server" | "managed_workload" | "unknown" | string;
  placement: CanonicalServicePlacement;
  service_key: string;
  service_instance: string;
  name: string;
  application_key?: string;
  application_display_name?: string;
  role?: string;
  lifecycle?: string;
  operational_impact?: string;
  internal_address?: string;
  runtime_identity?: Record<string, string>;
  /** Persisted ownership dimension; always present on this route. */
  management_state: RegistryManagementState;
  /**
   * The owner guardrail. Orthogonal to the measured dimensions: a locked
   * service keeps running and keeps reporting it — only `allowed_actions`
   * narrows. Optional so a backend that predates the field reads as unlocked.
   */
  mutation_lock?: CanonicalServiceMutationLock;
  desired_state: string;
  observed_state: string;
  health: CanonicalServiceHealth;
  stackkit_version?: string;
  catalog?: ServiceCatalogRef | null;
  version?: ServiceVersionInfo | null;
  placement_class?: ServicePlacementClass | string;
  access: Record<string, unknown>;
  allowed_actions: string[];
  inventory_revision: number;
  evidence_ref?: string;
  source: string;
  provenance: Record<string, unknown>;
  created_at: string;
  updated_at: string;
}

export interface StackItAvailability {
  available: boolean;
  reason_code?: string;
  name?: string;
  state?: "stacked" | "restored" | "released" | string;
  retain_until?: string;
  restore_available: boolean;
  release_original_available: boolean;
}

export interface StackItPlanRow {
  /** Stable machine code; the dialog translates by code and values. */
  code: string;
  values?: Record<string, string>;
  field: string;
  status: "kept" | "changed" | "refused" | string;
  observed?: string;
  adopted?: string;
  reason?: string;
}

export interface StackItPublishedPort {
  hostIP?: string;
  hostPort: number;
  containerPort: number;
  protocol: string;
}

/** Secret-free projection of stackkit.own-application-adoption/v1. */
export interface StackItPlan {
  outcome: string;
  admissible: boolean;
  planDigest?: string;
  name?: string;
  containerName: string;
  rows: StackItPlanRow[];
  hiddenKeys: string[];
  secretKeys: string[];
  publishedPorts: StackItPublishedPort[];
  data?: {
    source: string;
    mountPath: string;
    measured: boolean;
    bytes?: number;
    freeBytes?: number;
  };
  downtimeSeconds?: number;
  retainUntil?: string;
  notice: string;
}

export interface ApplicationAdoptionPlan {
  plan_id: string;
  service_id: string;
  status: string;
  error?: string;
  mode?: "adopt_in_place" | "stack_it";
  stack_plan?: StackItPlan;
  assessment?: {
    planDigest: string;
    profileRef: string;
    mode: string;
    source: { containerId: string; imageDigest: string; configMount: string };
    preservedFields: string[];
    admittedOperations: string[];
  };
}
/** A data checkpoint of an own application's tag update. */
export interface OwnApplicationCheckpoint {
  operation_id: string;
  state: string;
  retained_bytes: number;
  /** When the update took the copy. */
  at?: string;
}

export type OwnApplicationAction =
  | "status"
  | "logs"
  | "backup"
  | "start"
  | "stop"
  | "restart"
  | "update"
  | "rollback"
  | "checkpoint_rollback"
  | "checkpoint_discard";

/** An application StackKits runs as an own application (adopted with Stack it). */
export interface OwnApplicationProjection {
  project_ref: string;
  name?: string;
  actions: OwnApplicationAction[];
  reason_code?: string;
  status?: {
    name: string;
    state?: string;
    image?: string;
    observed: boolean;
    containers?: { service: string; state: string; health?: string }[];
    checkpoint?: OwnApplicationCheckpoint;
    job_id: string;
    at: string;
  };
}

export type OwnApplicationOperation =
  | "status"
  | "logs"
  | "backup"
  | "review"
  | "update_review"
  | "checkpoint_rollback_review"
  | "checkpoint_discard_review";

export interface OwnApplicationOperationRequest {
  operation: OwnApplicationOperation;
  action?: "start" | "stop" | "restart" | "rollback";
  image_tag?: string;
  allow_major?: boolean;
  checkpoint?: string;
  owner_approved?: boolean;
}

/** The projected node answer of one own application step. */
export interface OwnApplicationAnswer {
  operation?: string;
  outcome?: string;
  reason_code?: string;
  review_digest?: string;
  reviewed_action?: string;
  state?: string;
  image?: string;
  logs?: string;
  allow_major?: boolean;
  target_check?: string;
  prior?: { image?: string; version?: string; resolved_image?: string };
  target?: { image?: string; version?: string; resolved_image?: string };
  checkpoint?: OwnApplicationCheckpoint;
  replaces_checkpoint?: OwnApplicationCheckpoint;
  snapshot_id?: string;
}

export interface OwnApplicationJob {
  job_id: string;
  service_id: string;
  operation: string;
  status: string;
  result?: OwnApplicationAnswer;
  error?: string;
}

export async function startOwnApplicationOperation(
  service: CanonicalService,
  request: OwnApplicationOperationRequest,
  key: string,
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/own-application/operations`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        ...request,
        expected_inventory_revision: service.inventory_revision,
      }),
    },
  );
  return response.data;
}

export async function readOwnApplicationOperation(
  serviceId: string,
  jobId: string,
) {
  const response = await fetchApi<OwnApplicationJob>(
    `/api/v1/services/${encodeURIComponent(serviceId)}/own-application/operations/${encodeURIComponent(jobId)}`,
  );
  return response.data;
}

export async function executeOwnApplicationOperation(
  service: CanonicalService,
  reviewJobId: string,
  reviewDigest: string,
  key: string,
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/own-application/operations/${encodeURIComponent(reviewJobId)}/execute`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        review_digest: reviewDigest,
        owner_approved: true,
        expected_inventory_revision: service.inventory_revision,
      }),
    },
  );
  return response.data;
}

export async function prepareApplicationAdoption(
  service: CanonicalService,
  key: string,
  mode: "adopt_in_place" | "stack_it" = "adopt_in_place",
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/adoption-plans`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        mode,
        expected_inventory_revision: service.inventory_revision,
      }),
    },
  );
  return response.data;
}
export async function readApplicationAdoption(
  serviceId: string,
  planId: string,
) {
  const response = await fetchApi<ApplicationAdoptionPlan>(
    `/api/v1/services/${encodeURIComponent(serviceId)}/adoption-plans/${encodeURIComponent(planId)}`,
  );
  return response.data;
}
export async function executeApplicationAdoption(
  service: CanonicalService,
  plan: ApplicationAdoptionPlan,
  key: string,
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/adoption-plans/${encodeURIComponent(plan.plan_id)}/execute`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        mode: plan.mode ?? "adopt_in_place",
        plan_digest:
          plan.mode === "stack_it"
            ? plan.stack_plan?.planDigest
            : plan.assessment?.planDigest,
        expected_inventory_revision: service.inventory_revision,
        owner_approved: true,
      }),
    },
  );
  return response.data;
}
export async function releaseApplicationAdoption(
  service: CanonicalService,
  key: string,
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/release`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        expected_inventory_revision: service.inventory_revision,
        owner_approved: true,
      }),
    },
  );
  return response.data;
}

/** Restore or remove the original container a Stack it kept. */
export async function resolveStackedOriginal(
  service: CanonicalService,
  mode: "restore_original" | "release_original",
  key: string,
) {
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/services/${encodeURIComponent(service.id)}/stack-it/original`,
    {
      method: "POST",
      headers: { "Idempotency-Key": key },
      body: JSON.stringify({
        mode,
        expected_inventory_revision: service.inventory_revision,
        owner_approved: true,
      }),
    },
  );
  return response.data;
}

type CanonicalServiceWire = Omit<
  CanonicalService,
  "kit_deployment_id" | "management_state"
> & {
  kit_deployment_id?: string;
  management_state: unknown;
};

function requireKitDeploymentId(
  kitDeploymentId: string | undefined,
  context: string,
): string {
  const value = kitDeploymentId?.trim();
  if (!value) throw new Error(`${context}: missing kit deployment identity`);
  return value;
}

const PLACEMENT_CLASSES: readonly ServicePlacementClass[] = [
  "local",
  "cloud",
  "managed",
  "unknown",
];

/** A recognised placement class, or undefined so callers fall back to heuristics. */
export function parsePlacementClass(
  value: unknown,
): ServicePlacementClass | undefined {
  if (typeof value !== "string") return undefined;
  const normalized = value.trim().toLowerCase();
  // `serverless` is a deprecated alias of managed on the Brand side.
  if (normalized === "serverless") return "managed";
  return PLACEMENT_CLASSES.find((candidate) => candidate === normalized);
}

function textField(
  record: Record<string, unknown>,
  key: string,
): string | undefined {
  const value = record[key];
  return typeof value === "string" && value.trim() ? value.trim() : undefined;
}

function normalizeCatalogRef(value: unknown): ServiceCatalogRef | undefined {
  if (!value || typeof value !== "object") return undefined;
  const record = value as Record<string, unknown>;
  const id = textField(record, "id");
  if (!id) return undefined;
  return {
    id,
    display_name: textField(record, "display_name"),
    category: textField(record, "category"),
    brand_domain: textField(record, "brand_domain"),
    web_port:
      typeof record.web_port === "number" && record.web_port > 0
        ? record.web_port
        : undefined,
    guide_url: httpsUrl(record.guide_url),
    vendor: normalizeVendor(record.vendor),
    clients: normalizeClients(record.clients),
    homepage_url: httpsUrl(record.homepage_url),
    evidence: normalizeEvidence(record.evidence),
    logo: normalizeLogo(record.logo),
    logo_removed: record.logo_removed === true || undefined,
  };
}

const CATALOG_EVIDENCE: readonly ServiceCatalogEvidence[] = [
  "label",
  "image",
  "name",
  "http_fingerprint",
  "released_catalog",
];

function normalizeEvidence(value: unknown): ServiceCatalogEvidence | undefined {
  return CATALOG_EVIDENCE.find((evidence) => evidence === value);
}

const LOGO_SOURCES: readonly ServiceCatalogLogoSource[] = [
  "dashboard-icons",
  "simple-icons",
  "kombify",
];

/**
 * A logo is only ever drawn from our own asset host: a URL that points
 * anywhere else is dropped, so the monogram shows instead of a hot-linked
 * third-party image.
 */
function normalizeLogo(value: unknown): ServiceCatalogLogo | undefined {
  if (!value || typeof value !== "object") return undefined;
  const record = value as Record<string, unknown>;
  const url = appIconUrlField(record.url);
  const source = LOGO_SOURCES.find((candidate) => candidate === record.source);
  if (!url || !source) return undefined;
  return {
    url,
    light_url: appIconUrlField(record.light_url),
    dark_url: appIconUrlField(record.dark_url),
    source,
  };
}

function appIconUrlField(value: unknown): string | undefined {
  return typeof value === "string" && isAppIconUrl(value) ? value : undefined;
}

function httpsUrl(value: unknown): string | undefined {
  return typeof value === "string" && value.startsWith("https://")
    ? value
    : undefined;
}

function normalizeVendor(value: unknown): ServiceVendor | undefined {
  if (!value || typeof value !== "object") return undefined;
  const record = value as Record<string, unknown>;
  const support = Array.isArray(record.support)
    ? record.support.flatMap((entry) => {
        const item = (entry ?? {}) as Record<string, unknown>;
        const url = httpsUrl(item.url);
        const kind = item.kind;
        return url && (kind === "donate" || kind === "sponsor" || kind === "purchase")
          ? [{ kind, url } as ServiceSupport]
          : [];
      })
    : undefined;
  const vendor: ServiceVendor = {
    name: textField(record, "name"),
    website: httpsUrl(record.website),
    docs: httpsUrl(record.docs),
    license: textField(record, "license"),
    support: support?.length ? support : undefined,
  };
  return Object.values(vendor).some(Boolean) ? vendor : undefined;
}

const CLIENT_PLATFORMS = ["android", "ios", "windows", "macos", "linux"];

function normalizeClients(value: unknown): ServiceClient[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const clients = value.flatMap((entry) => {
    const item = (entry ?? {}) as Record<string, unknown>;
    const url = httpsUrl(item.url);
    if (
      !url ||
      !CLIENT_PLATFORMS.includes(String(item.platform)) ||
      (item.kind !== "store" && item.kind !== "download")
    )
      return [];
    return [
      { platform: item.platform, kind: item.kind, url } as ServiceClient,
    ];
  });
  return clients.length ? clients : undefined;
}

function normalizeVersionInfo(value: unknown): ServiceVersionInfo | undefined {
  if (!value || typeof value !== "object") return undefined;
  const record = value as Record<string, unknown>;
  const version: ServiceVersionInfo = {
    current: textField(record, "current"),
    source: textField(record, "source"),
    image: textField(record, "image"),
    latest: textField(record, "latest"),
    update_available: record.update_available === true,
    checked_at: textField(record, "checked_at"),
    update_mode: textField(record, "update_mode"),
    reason_code: textField(record, "reason_code"),
    evidence_status: textField(record, "evidence_status"),
    evidence_reason_code: textField(record, "evidence_reason_code"),
    observed_at: textField(record, "observed_at"),
    update_setting: textField(record, "update_setting"),
    latest_digest: textField(record, "latest_digest"),
    tested: typeof record.tested === "boolean" ? record.tested : undefined,
    release_notes_url: httpsField(record, "release_notes_url"),
    newer_versions: offerList(record.newer_versions),
    advisories: advisoryList(record.advisories),
    security_fix: securityFix(record.security_fix),
  };
  // A version block with nothing measured carries no information.
  return version.current ||
    version.latest ||
    version.image ||
    version.reason_code ||
    version.evidence_status
    ? version
    : undefined;
}

function httpsField(
  record: Record<string, unknown>,
  key: string,
): string | undefined {
  const value = textField(record, key);
  return value?.startsWith("https://") ? value : undefined;
}

function offerOf(value: unknown): ApplicationVersionOffer | undefined {
  if (!value || typeof value !== "object") return undefined;
  const record = value as Record<string, unknown>;
  const version = textField(record, "version");
  if (!version) return undefined;
  return {
    version,
    digest: textField(record, "digest"),
    tested: record.tested === true,
    release_notes_url: httpsField(record, "release_notes_url"),
    published_at: textField(record, "published_at"),
  };
}

function offerList(value: unknown): ApplicationVersionOffer[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const offers = value.flatMap((item) => offerOf(item) ?? []);
  return offers.length ? offers : undefined;
}

function securityFix(
  value: unknown,
): (ApplicationVersionOffer & { advisories: string[] }) | undefined {
  const offer = offerOf(value);
  if (!offer) return undefined;
  const ids = (value as Record<string, unknown>).advisories;
  return {
    ...offer,
    advisories: Array.isArray(ids)
      ? ids.filter((id): id is string => typeof id === "string")
      : [],
  };
}

function advisoryList(value: unknown): ApplicationAdvisory[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const advisories = value.flatMap((item): ApplicationAdvisory[] => {
    if (!item || typeof item !== "object") return [];
    const record = item as Record<string, unknown>;
    const id = textField(record, "id");
    const url = httpsField(record, "url");
    if (!id) return [];
    return [
      {
        id,
        severity: textField(record, "severity") ?? "unknown",
        cvss: typeof record.cvss === "number" ? record.cvss : undefined,
        fixed_version: textField(record, "fixed_version"),
        url: url ?? "",
        affects: textField(record, "affects") ?? "unknown",
        high: record.high === true,
      },
    ];
  });
  return advisories.length ? advisories : undefined;
}

function stringList(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((entry): entry is string => typeof entry === "string")
    : [];
}

/**
 * Normalize one canonical service. Identity and ownership are contract
 * violations that throw; every other field is optional so a backend that
 * predates it (or omits it for one row) still renders.
 */
function normalizeCanonicalService(
  service: CanonicalServiceWire,
  context: string,
): CanonicalService {
  const { kit_deployment_id, ...result } = service;
  return {
    ...result,
    kit_deployment_id: requireKitDeploymentId(kit_deployment_id, context),
    management_state: parseManagementState(service.management_state, context),
    catalog: normalizeCatalogRef(service.catalog) ?? null,
    version: normalizeVersionInfo(service.version) ?? null,
    placement_class: parsePlacementClass(service.placement_class),
    allowed_actions: stringList(service.allowed_actions),
    access:
      service.access && typeof service.access === "object"
        ? service.access
        : {},
    health:
      service.health && typeof service.health === "object"
        ? service.health
        : { state: "" },
    placement: {
      ...service.placement,
      freshness: service.placement?.freshness ?? { state: "unknown" },
    },
  };
}

export interface ServiceApplicationAccess {
  kind: "public" | "internal" | "unavailable" | string;
  address?: string;
  open_url?: string;
  reason?: string;
}

export interface ServiceApplication {
  application_id: string;
  kit_deployment_id: string;
  server_id: string;
  application_key: string;
  display_name: string;
  status: string;
  access: ServiceApplicationAccess;
  components: CanonicalService[];
  observed_at?: string;
  system: boolean;
  provenance: Record<string, unknown>;
  /** Union of the actions that apply app-wide (additive backend field). */
  allowed_actions?: string[];
  catalog?: ServiceCatalogRef | null;
  /** Version of the primary component. */
  version?: ServiceVersionInfo | null;
  placement_class?: ServicePlacementClass | string;
}

type ServiceApplicationWire = Omit<
  ServiceApplication,
  "kit_deployment_id" | "components"
> & {
  kit_deployment_id?: string;
  components?: CanonicalServiceWire[];
};

/** A row the client could not read; reported, never fatal for the list. */
export interface ServiceRowRejection {
  index: number;
  id?: string;
  reason: string;
}

export interface ServiceRowsResult<T> {
  items: T[];
  rejected: ServiceRowRejection[];
}

function rowId(value: unknown, keys: string[]): string | undefined {
  if (!value || typeof value !== "object") return undefined;
  for (const key of keys) {
    const candidate = (value as Record<string, unknown>)[key];
    if (typeof candidate === "string" && candidate) return candidate;
  }
  return undefined;
}

/**
 * Normalize a list row by row. One malformed row (unknown ownership, empty
 * deployment identity) is dropped and reported instead of blanking the list.
 */
function normalizeRows<Wire, T>(
  rows: Wire[] | null | undefined,
  idKeys: string[],
  normalize: (row: Wire, index: number) => T,
): ServiceRowsResult<T> {
  const items: T[] = [];
  const rejected: ServiceRowRejection[] = [];
  (rows ?? []).forEach((row, index) => {
    try {
      items.push(normalize(row, index));
    } catch (error) {
      rejected.push({
        index,
        id: rowId(row, idKeys),
        reason: error instanceof Error ? error.message : String(error),
      });
    }
  });
  return { items, rejected };
}

function normalizeServiceApplication(
  application: ServiceApplicationWire,
  context: string,
  rejected?: ServiceRowRejection[],
): ServiceApplication {
  const { kit_deployment_id, components, ...result } = application;
  const normalizedComponents = normalizeRows(
    components,
    ["id"],
    (component: CanonicalServiceWire, index) =>
      normalizeCanonicalService(component, `${context}.components[${index}]`),
  );
  rejected?.push(...normalizedComponents.rejected);
  return {
    ...result,
    kit_deployment_id: requireKitDeploymentId(kit_deployment_id, context),
    components: normalizedComponents.items,
    access:
      application.access && typeof application.access === "object"
        ? application.access
        : { kind: "unavailable" },
    system: application.system === true,
    provenance: application.provenance ?? {},
    allowed_actions: stringList(application.allowed_actions),
    catalog: normalizeCatalogRef(application.catalog) ?? null,
    version: normalizeVersionInfo(application.version) ?? null,
    placement_class: parsePlacementClass(application.placement_class),
  };
}

/** Applications plus the rows that could not be read. */
export async function listServiceApplicationsReport(
  kitDeploymentId?: string,
): Promise<ServiceRowsResult<ServiceApplication>> {
  const query = kitDeploymentId
    ? `?kit_deployment_id=${encodeURIComponent(kitDeploymentId)}`
    : "";
  const response = await fetchApi<ServiceApplicationWire[]>(
    `/api/v1/service-applications${query}`,
  );
  const componentRejections: ServiceRowRejection[] = [];
  const result = normalizeRows(
    response.data,
    ["application_id", "application_key"],
    (application: ServiceApplicationWire, index) =>
      normalizeServiceApplication(
        application,
        `service-applications[${index}]`,
        componentRejections,
      ),
  );
  return {
    items: result.items,
    rejected: [...result.rejected, ...componentRejections],
  };
}

export async function listServiceApplications(
  kitDeploymentId?: string,
): Promise<ServiceApplication[]> {
  return (await listServiceApplicationsReport(kitDeploymentId)).items;
}

export async function getServiceApplication(
  applicationId: string,
): Promise<ServiceApplication> {
  const response = await fetchApi<ServiceApplicationWire>(
    `/api/v1/service-applications/${encodeURIComponent(applicationId)}`,
  );
  return normalizeServiceApplication(
    response.data,
    `service-applications/${applicationId}`,
  );
}

/** Canonical services plus the rows that could not be read. */
export async function listCanonicalServicesReport(
  kitDeploymentId?: string,
): Promise<ServiceRowsResult<CanonicalService>> {
  const query = kitDeploymentId
    ? `?kit_deployment_id=${encodeURIComponent(kitDeploymentId)}`
    : "";
  const response = await fetchApi<CanonicalServiceWire[]>(
    `/api/v1/services${query}`,
  );
  return normalizeRows(
    response.data,
    ["id"],
    (service: CanonicalServiceWire, index) =>
      normalizeCanonicalService(service, `services[${index}]`),
  );
}

/** List the canonical service aggregates for the caller's tenant. */
export async function listCanonicalServices(
  kitDeploymentId?: string,
): Promise<CanonicalService[]> {
  return (await listCanonicalServicesReport(kitDeploymentId)).items;
}

/** Read one canonical service aggregate. */
export async function getCanonicalService(
  serviceId: string,
): Promise<CanonicalService> {
  const response = await fetchApi<CanonicalServiceWire>(
    `/api/v1/services/${encodeURIComponent(serviceId)}`,
  );
  return normalizeCanonicalService(response.data, `services/${serviceId}`);
}

export interface ManagedServiceActionResponse {
  /** The durable job, or the action card's execution when action_card_id is set. */
  job_id: string;
  /** Set when a mutation (start, stop, restart, update, backup) runs as a governed RIL action card. */
  action_card_id?: string;
  service_id: string;
  action: ManagedServiceAction;
  status: string;
}

/** The public state of one governed RIL action card. */
export interface GovernedActionCard {
  id: string;
  status: string;
  execution_id?: string;
  error_code?: string;
  created_at: string;
  updated_at: string;
  evidence?: {
    status?: string;
    summary_codes?: string[];
  };
}

/** Read one governed action card of the signed-in owner. */
export async function getGovernedActionCard(
  cardId: string,
): Promise<GovernedActionCard> {
  const response = await fetchApi<GovernedActionCard>(
    `/v1/ril/action-cards/${encodeURIComponent(cardId)}`,
  );
  return response.data;
}

export interface ManagedServiceLogEntry {
  timestamp: string;
  message: string;
}

export interface ManagedServiceLogsResponse {
  service_id: string;
  job_id?: string;
  status: "queued" | "running" | "completed" | "empty" | string;
  entries: ManagedServiceLogEntry[];
  next_cursor?: string;
}

export interface ManagedServiceLogOptions {
  /** The service API contract deliberately bounds a single page to 200 rows. */
  limit?: number;
  cursor?: string;
}

const defaultServiceLogLimit = 100;
const maxServiceLogLimit = 200;

function normalizeServiceLogOptions(
  options: ManagedServiceLogOptions = {},
): Required<ManagedServiceLogOptions> {
  const limit = options.limit ?? defaultServiceLogLimit;
  if (!Number.isInteger(limit) || limit < 1 || limit > maxServiceLogLimit) {
    throw new RangeError(
      `Service log limit must be an integer between 1 and ${maxServiceLogLimit}`,
    );
  }
  return { limit, cursor: options.cursor?.trim() ?? "" };
}

/**
 * The release one application update moves to (update fact
 * `application_update`). The server re-checks it against a fresh fact: an
 * unchecked version needs the owner's acknowledgement, a security fix names
 * the advisory it closes.
 */
export interface ApplicationUpdateSelection {
  version: string;
  digest: string;
  acknowledge_untested?: boolean;
  advisory_id?: string;
}

/** Outcome of one application update, from its job result. */
export interface ApplicationUpdateOutcome {
  result: "updated" | "rolled_back" | "rollback_failed" | "refused" | string;
  reason_code?: string;
  prior?: { version: string; digest?: string };
  target?: { version: string; digest?: string };
}

export async function runManagedServiceAction(
  serviceId: string,
  action: ManagedServiceAction,
  expectedInventoryRevision: number,
  idempotencyKey: string,
  logOptions?: ManagedServiceLogOptions,
  update?: ApplicationUpdateSelection,
): Promise<ManagedServiceActionResponse> {
  if (action === "logs") {
    // A log read is its own read operation, never a service action.
    const { limit, cursor } = normalizeServiceLogOptions(logOptions);
    const logsResponse = await fetchApi<ManagedServiceActionResponse>(
      `/api/v1/registry/services/${encodeURIComponent(serviceId)}/logs`,
      {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          "Idempotency-Key": idempotencyKey,
        },
        body: JSON.stringify({
          expected_inventory_revision: expectedInventoryRevision,
          limit,
          ...(cursor ? { cursor } : {}),
        }),
      },
    );
    return logsResponse.data;
  }
  // The lock is not bound to a runtime observation - sending a revision would
  // claim a guarantee the endpoint does not make, and it rejects one. No
  // request carries an approval flag: every mutation is one governed action
  // card whose Decision the acting agent's equipment answers.
  const body: Record<string, unknown> = SERVICE_LOCK_ACTIONS.has(action)
    ? { action }
    : {
        action,
        expected_inventory_revision: expectedInventoryRevision,
      };
  if (action === "update" && update) {
    body.version = update.version;
    body.digest = update.digest;
    if (update.acknowledge_untested) body.acknowledge_untested = true;
    if (update.advisory_id) body.advisory_id = update.advisory_id;
  }
  const response = await fetchApi<ManagedServiceActionResponse>(
    `/api/v1/registry/services/${encodeURIComponent(serviceId)}/actions`,
    {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        "Idempotency-Key": idempotencyKey,
      },
      body: JSON.stringify(body),
    },
  );
  return response.data;
}

/**
 * Read the redacted, bounded log page produced by a completed `logs` action.
 * The backend owns log collection and redaction; the client only follows its
 * opaque cursor and never requests an unbounded transcript.
 */
export async function getManagedServiceLogs(
  serviceId: string,
  options?: ManagedServiceLogOptions,
): Promise<ManagedServiceLogsResponse> {
  const { limit, cursor } = normalizeServiceLogOptions(options);
  const query = new URLSearchParams({ limit: String(limit) });
  if (cursor) query.set("cursor", cursor);
  const response = await fetchApi<ManagedServiceLogsResponse>(
    `/api/v1/registry/services/${encodeURIComponent(serviceId)}/logs?${query.toString()}`,
  );
  return response.data;
}
