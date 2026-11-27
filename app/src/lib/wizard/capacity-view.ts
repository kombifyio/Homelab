/**
 * The capacity advice of the Unifier (`capacity` of the recommendations
 * answer) as what the creation screens show. The backend owns the model and
 * every number (MiB, advisory, never a gate); this file only turns the answer
 * into a hint and the host the person chose into the request. It estimates
 * nothing.
 */
import type {
  WizardCapacity,
  WizardCapacityDriver,
  WizardRecommendationRequest,
} from "#lib/api/unifier.js";
import { normalizeInstallableKitSlug } from "#lib/wizard/standardBundle.js";
import {
  readRuntimeOfferingID,
  type RuntimeOfferingID,
  type StackConfig,
} from "#lib/wizard/types.js";

const MIB_PER_GB = 1024;

/** A managed size that carries the selection. */
export interface CapacitySize {
  id: RuntimeOfferingID;
  /** Memory in gigabytes, as the offering declares it. */
  gb: number;
}

export interface CapacityHint {
  /** `estimate`: the selection has a need estimate but no server is selected yet. */
  status: "fits" | "tight" | "estimate" | "unknown";
  /** Gigabytes the selection needs at most, and the server's. */
  needGb: number;
  haveGb: number;
  /** 0..1 share of the server's memory the selection takes (capped). */
  share: number;
  /** The need is only a lower bound: some use case has no known peak. */
  atLeast: boolean;
  /** The provider's own offering that carries the selection (real size). */
  size?: CapacitySize;
  /** Use cases whose omission makes the selection fit, in the backend's order. */
  omit: string[];
  /** Combined RAM of approved existing servers, when Step 1 has no chosen host. */
  existingHaveGb?: number;
  /** Use cases and settings that materially change capacity demand. */
  drivers: WizardCapacityDriver[];
}

export function toGb(mib: number): number {
  return mib / MIB_PER_GB;
}

function readableOffering(id: string): RuntimeOfferingID | undefined {
  try {
    return readRuntimeOfferingID(id);
  } catch {
    return undefined;
  }
}

/** The backend's offering with its real size, when it is a different one. */
function namedSize(
  capacity: WizardCapacity,
  currentOfferingId: string | undefined,
): CapacitySize | undefined {
  const id = capacity.suggested_offering_id
    ? readableOffering(capacity.suggested_offering_id)
    : undefined;
  const mib = capacity.suggested_offering_ram_mb ?? 0;
  if (!id || id === currentOfferingId || !(mib > 0)) return undefined;
  return { id, gb: toGb(mib) };
}

/** The backend could not size every use case: its need is only a lower bound. */
function lowerBound(capacity: WizardCapacity): boolean {
  return capacity.reasons.some(
    (reason) => reason.code === "capacity_peak_unknown",
  );
}

const NOT_ASSESSED: CapacityHint = {
  status: "unknown",
  needGb: 0,
  haveGb: 0,
  share: 0,
  atLeast: false,
  omit: [],
  drivers: [],
};

/**
 * What the hint shows. `assessed`: the question for this server and selection
 * was answered (or failed), so an answer that cannot say whether it fits reads
 * "not assessed" instead of staying silent; before that, nothing is shown. The
 * advice never fabricates a fit: a need with an unknown peak is a lower bound
 * that can prove a misfit but never a fit.
 */
export function capacityHint(
  capacity: WizardCapacity | undefined,
  options: {
    currentOfferingId?: string;
    managed: boolean;
    assessed: boolean;
    existingRamMb?: number;
  },
): CapacityHint | undefined {
  const silent = options.assessed ? NOT_ASSESSED : undefined;
  if (
    !capacity ||
    (capacity.status === "unknown" &&
      (!(capacity.required_ram_mb > 0) || capacity.available_ram_mb > 0))
  )
    return silent;
  const atLeast = lowerBound(capacity);
  if (atLeast && capacity.status !== "warning" && capacity.available_ram_mb > 0)
    return silent;
  if (!(capacity.required_ram_mb > 0)) return silent;
  if (!(capacity.available_ram_mb > 0)) {
    const existingHaveGb =
      options.existingRamMb && options.existingRamMb > 0
        ? toGb(options.existingRamMb)
        : undefined;
    return {
      status: "estimate",
      needGb: toGb(capacity.required_ram_mb),
      haveGb: 0,
      share: 0,
      atLeast,
      omit: [],
      existingHaveGb,
      drivers: (capacity.drivers ?? []).slice(0, 3),
    };
  }
  if (atLeast && capacity.status !== "warning") return silent;
  const tight = capacity.status === "warning";
  return {
    status: tight ? "tight" : "fits",
    needGb: toGb(capacity.required_ram_mb),
    haveGb: toGb(capacity.available_ram_mb),
    share: Math.min(capacity.required_ram_mb / capacity.available_ram_mb, 1),
    size:
      tight && options.managed && !atLeast
        ? namedSize(capacity, options.currentOfferingId)
        : undefined,
    atLeast,
    omit: tight && !atLeast ? (capacity.omit_goals ?? []) : [],
    drivers: (capacity.drivers ?? []).slice(0, 3),
  };
}

/**
 * The part of the recommendations request that names the chosen server. A
 * managed server is named by provider and size and sized by the backend (the
 * provider's package, not the generic offering row); a hypervisor guest by its
 * chosen size; a server that is not enrolled yet has no known size.
 */
export function capacityRequestHost(
  config: StackConfig,
  options: {
    /** Step 1 must not treat the default managed offering as selected. */
    allowManagedOffering?: boolean;
    selectedServerRamMb?: number;
  } = {},
): Pick<
  WizardRecommendationRequest,
  "server_capacity" | "runtime_offering_id" | "kit_slug"
> {
  if (options.selectedServerRamMb && options.selectedServerRamMb > 0)
    return {
      server_capacity: { ram_mb: options.selectedServerRamMb },
      ...kitIdentity(config),
    };
  const server = config.serverProvisioning;
  if (
    options.allowManagedOffering !== false &&
    server.mode === "kombify-cloud" &&
    config.runtimeOfferingId
  )
    return { runtime_offering_id: config.runtimeOfferingId };
  const guest = server.mode === "hypervisor" ? server.substrate : undefined;
  if (guest && guest.memoryMiB > 0)
    return {
      server_capacity: {
        ram_mb: guest.memoryMiB,
        ...(guest.cpu > 0 ? { cpu_cores: guest.cpu } : {}),
        ...(guest.diskGiB > 0 ? { disk_gb: guest.diskGiB } : {}),
      },
      ...kitIdentity(config),
    };
  return {};
}

/**
 * The kit the own server will run, by the same mapping the run request uses
 * (`normalizeRunKitSlug`); a core's memory need differs per kit. Any other
 * kit leaves it out and the backend says it cannot assess.
 */
function kitIdentity(config: StackConfig): { kit_slug?: string } {
  const slug = normalizeInstallableKitSlug(
    config.serverProvisioning.stackkitFoundation || config.kit || "",
    false,
  );
  return slug === "cloud-kit" || slug === "basement-kit"
    ? { kit_slug: slug }
    : {};
}
