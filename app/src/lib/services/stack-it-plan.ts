import { tr, formatNumber, type MessageKey } from "#lib/i18n.svelte.js";
import { ApiRequestError } from "#lib/api/client.js";
import type { StackItPlanRow } from "#lib/api/services.js";

/**
 * Localized text for the rows of a StackKits "Stack it" plan
 * (stackkit.own-application-adoption/v1). Every row carries a stable machine
 * code and its structured values (StackKits docs/APPLICATION_ADOPTION.md,
 * "Plan row codes"); the dialog translates by code and fills in the values.
 * The producer's English reason is never shown. A code this version does not
 * know yet shows one generic localized line.
 */
const ROW_KEYS: Record<string, MessageKey> = {
  image_pinned_by_digest: "ui.stackIt.row.image_pinned_by_digest",
  volume_copied_checksummed: "ui.stackIt.row.volume_copied_checksummed",
  data_size_measured: "ui.stackIt.row.data_size_measured",
  data_size_not_measured: "ui.stackIt.row.data_size_not_measured",
  environment_empty_kept: "ui.stackIt.row.environment_empty_kept",
  environment_values_to_custody: "ui.stackIt.row.environment_values_to_custody",
  secrets_classified: "ui.stackIt.row.secrets_classified",
  http_port_routed: "ui.stackIt.row.http_port_routed",
  port_kept: "ui.stackIt.row.port_kept",
  port_random_dropped: "ui.stackIt.row.port_random_dropped",
  port_ipv6_only_dropped: "ui.stackIt.row.port_ipv6_only_dropped",
  port_router_owned_dropped: "ui.stackIt.row.port_router_owned_dropped",
  port_dropped_v3_project: "ui.stackIt.row.port_dropped_v3_project",
  outbound_none_kept: "ui.stackIt.row.outbound_none_kept",
  outbound_kept_firewalled: "ui.stackIt.row.outbound_kept_firewalled",
  outbound_turned_off: "ui.stackIt.row.outbound_turned_off",
  restart_unless_stopped: "ui.stackIt.row.restart_unless_stopped",
  restart_by_firewall_watcher: "ui.stackIt.row.restart_by_firewall_watcher",
  own_private_network: "ui.stackIt.row.own_private_network",
  hostname_route_with_sign_in: "ui.stackIt.row.hostname_route_with_sign_in",
  container_recreated: "ui.stackIt.row.container_recreated",
  downtime_estimated: "ui.stackIt.row.downtime_estimated",
  readiness_answered: "ui.stackIt.row.readiness_answered",
  readiness_wrong_status: "ui.stackIt.row.readiness_wrong_status",
  readiness_no_answer: "ui.stackIt.row.readiness_no_answer",
  readiness_not_probed: "ui.stackIt.row.readiness_not_probed",
  healthcheck_from_image: "ui.stackIt.row.healthcheck_from_image",
  cgroupns_daemon_default: "ui.stackIt.row.cgroupns_daemon_default",
  read_only_root_dropped: "ui.stackIt.row.read_only_root_dropped",
  capabilities_fixed_policy: "ui.stackIt.row.capabilities_fixed_policy",
  host_override_dropped: "ui.stackIt.row.host_override_dropped",
  tmpfs_mount_dropped: "ui.stackIt.row.tmpfs_mount_dropped",
  timezone_mount_dropped: "ui.stackIt.row.timezone_mount_dropped",
  resources_bounded: "ui.stackIt.row.resources_bounded",
  data_volume_replaced: "ui.stackIt.row.data_volume_replaced",
  compose_single_service_adopted: "ui.stackIt.row.compose_single_service_adopted",
  compose_project_adopted: "ui.stackIt.row.compose_project_adopted",
  refused_already_managed: "ui.stackIt.row.refused_already_managed",
  refused_compose_project: "ui.stackIt.row.refused_compose_project",
  refused_compose_too_many_services: "ui.stackIt.row.refused_compose_too_many_services",
  refused_shared_user_network: "ui.stackIt.row.refused_shared_user_network",
  refused_local_only_image: "ui.stackIt.row.refused_local_only_image",
  refused_image_reference: "ui.stackIt.row.refused_image_reference",
  refused_registry_image: "ui.stackIt.row.refused_registry_image",
  refused_custom_command: "ui.stackIt.row.refused_custom_command",
  refused_user_override: "ui.stackIt.row.refused_user_override",
  refused_host_network: "ui.stackIt.row.refused_host_network",
  refused_container_network: "ui.stackIt.row.refused_container_network",
  refused_privileged: "ui.stackIt.row.refused_privileged",
  refused_added_capabilities: "ui.stackIt.row.refused_added_capabilities",
  refused_devices: "ui.stackIt.row.refused_devices",
  refused_shared_namespace: "ui.stackIt.row.refused_shared_namespace",
  refused_relaxed_security: "ui.stackIt.row.refused_relaxed_security",
  refused_volumes_from: "ui.stackIt.row.refused_volumes_from",
  refused_custom_runtime: "ui.stackIt.row.refused_custom_runtime",
  refused_legacy_links: "ui.stackIt.row.refused_legacy_links",
  refused_host_user_namespace: "ui.stackIt.row.refused_host_user_namespace",
  refused_docker_socket: "ui.stackIt.row.refused_docker_socket",
  refused_bind_outside_data_roots: "ui.stackIt.row.refused_bind_outside_data_roots",
  refused_mount_type: "ui.stackIt.row.refused_mount_type",
  refused_multiple_data_mounts: "ui.stackIt.row.refused_multiple_data_mounts",
  refused_image_volume: "ui.stackIt.row.refused_image_volume",
  refused_environment_entry: "ui.stackIt.row.refused_environment_entry",
  refused_multiline_environment: "ui.stackIt.row.refused_multiline_environment",
  refused_environment_value_size: "ui.stackIt.row.refused_environment_value_size",
  refused_environment_count: "ui.stackIt.row.refused_environment_count",
  refused_unknown_secret_env: "ui.stackIt.row.refused_unknown_secret_env",
  refused_http_port_ambiguous: "ui.stackIt.row.refused_http_port_ambiguous",
  refused_http_service_ambiguous: "ui.stackIt.row.refused_http_service_ambiguous",
  refused_name: "ui.stackIt.row.refused_name",
  refused_existing_application_volume: "ui.stackIt.row.refused_existing_application_volume",
  refused_data_unmeasurable: "ui.stackIt.row.refused_data_unmeasurable",
  refused_data_disk_space: "ui.stackIt.row.refused_data_disk_space",
  refused_contract: "ui.stackIt.row.refused_contract",
  refused_review: "ui.stackIt.row.refused_review",
  refused_review_outbound: "ui.stackIt.row.refused_review_outbound",
};

/** Human size of a byte count the producer sends as a decimal string. */
export function byteSize(bytes?: number | string): string {
  const value = typeof bytes === "string" ? Number(bytes) : bytes;
  if (value === undefined || !Number.isFinite(value)) return "?";
  const units = ["B", "KB", "MB", "GB", "TB"];
  let scaled = value;
  let unit = 0;
  while (scaled >= 1024 && unit < units.length - 1) {
    scaled /= 1024;
    unit += 1;
  }
  return `${formatNumber(scaled, { maximumFractionDigits: 1 })} ${units[unit]}`;
}

const reported = new Set<string>();

/**
 * The localized explanation of one plan row from its code and values. Byte
 * counts are shown as sizes; a missing value reads as "?".
 */
export function stackItRowText(row: Pick<StackItPlanRow, "code" | "values">): string {
  const values: Record<string, string> = { ...row.values };
  for (const key of ["bytes", "freeBytes"]) {
    if (values[key] !== undefined) values[key] = byteSize(values[key]);
  }
  const rowKey = ROW_KEYS[row.code];
  if (rowKey) {
    return tr(rowKey, values);
  }
  if (!reported.has(row.code)) {
    reported.add(row.code);
    console.warn("stack-it: plan row code without a message", row.code);
  }
  return tr("ui.stackIt.row.unknown");
}

/** A failed Stack it job: its structured outcome and reason codes. */
export class StackItJobError extends Error {
  constructor(
    readonly outcome: string,
    readonly reasonCode?: string,
    readonly rowCode?: string,
  ) {
    super(`stack it ${outcome}`);
  }
}

/** Reads the structured failure a Stack it job records in its result. */
export function stackItJobError(result: unknown): StackItJobError {
  const failure =
    result && typeof result === "object"
      ? ((result as Record<string, unknown>).application_stack_failure as
          | Record<string, unknown>
          | undefined)
      : undefined;
  const text = (key: string) =>
    typeof failure?.[key] === "string" ? (failure[key] as string) : undefined;
  return new StackItJobError(
    text("outcome") ?? "",
    text("reason_code"),
    text("row_code"),
  );
}

const OUTCOMES: Record<string, MessageKey> = {
  rolled_back: "ui.stackIt.error.rolledBack",
  rollback_failed: "ui.stackIt.error.rollbackFailed",
  refused: "ui.stackIt.error.refused",
  unhealthy: "ui.stackIt.error.unhealthy",
};

const REASON_CODES: Record<string, MessageKey> = {
  application_stack_unavailable: "ui.stackIt.error.unavailable",
  application_adoption_unavailable: "ui.stackIt.error.unavailable",
  application_stacked: "ui.stackIt.error.alreadyStacked",
  application_stack_retention_ended: "ui.stackIt.error.retentionEnded",
  application_stack_name_unavailable: "ui.stackIt.error.refused",
  application_source_identity_unavailable: "ui.stackIt.error.refused",
  application_adoption_profile_unsupported: "ui.stackIt.error.refused",
  application_source_changed: "ui.stackIt.error.changed",
  application_intent_changed: "ui.stackIt.error.changed",
  service_locked: "ui.stackIt.error.locked",
};

/**
 * One localized line for a failed Stack it request or job, from structured
 * codes only: an HTTP reason_code, or the job's outcome, reason code and
 * first refused row. Producer and HTTP text never reach the person.
 */
export function stackItFailure(error: unknown): string {
  if (error instanceof ApiRequestError) {
    const details = error.details as { reason_code?: unknown } | undefined;
    const code =
      typeof details?.reason_code === "string" ? details.reason_code : "";
    const key = REASON_CODES[code];
    if (key) return tr(key);
    if (error.status === 409) return tr("ui.stackIt.error.changed");
    if (error.status === 503) return tr("ui.stackIt.error.unavailable");
    return tr("ui.stackIt.error.failed");
  }
  if (error instanceof StackItJobError) {
    const reason = error.reasonCode ? REASON_CODES[error.reasonCode] : undefined;
    if (reason) return tr(reason);
    const outcome = OUTCOMES[error.outcome];
    if (outcome) {
      const line = tr(outcome);
      return error.rowCode && error.outcome === "refused"
        ? `${line} ${stackItRowText({ code: error.rowCode })}`
        : line;
    }
  }
  return tr("ui.stackIt.error.failed");
}
