import { tr, formatNumber } from "#lib/i18n.svelte.js";
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
const ROW_CODES = new Set([
  "image_pinned_by_digest",
  "volume_copied_checksummed",
  "data_size_measured",
  "data_size_not_measured",
  "environment_empty_kept",
  "environment_values_to_custody",
  "secrets_classified",
  "http_port_routed",
  "port_kept",
  "port_random_dropped",
  "port_ipv6_only_dropped",
  "port_router_owned_dropped",
  "port_dropped_v3_project",
  "outbound_none_kept",
  "outbound_kept_firewalled",
  "outbound_turned_off",
  "restart_unless_stopped",
  "restart_by_firewall_watcher",
  "own_private_network",
  "hostname_route_with_sign_in",
  "container_recreated",
  "downtime_estimated",
  "readiness_answered",
  "readiness_wrong_status",
  "readiness_no_answer",
  "readiness_not_probed",
  "healthcheck_from_image",
  "cgroupns_daemon_default",
  "read_only_root_dropped",
  "capabilities_fixed_policy",
  "host_override_dropped",
  "tmpfs_mount_dropped",
  "timezone_mount_dropped",
  "resources_bounded",
  "data_volume_replaced",
  "compose_single_service_adopted",
  "compose_project_adopted",
  "refused_already_managed",
  "refused_compose_project",
  "refused_compose_too_many_services",
  "refused_shared_user_network",
  "refused_local_only_image",
  "refused_image_reference",
  "refused_registry_image",
  "refused_custom_command",
  "refused_user_override",
  "refused_host_network",
  "refused_container_network",
  "refused_privileged",
  "refused_added_capabilities",
  "refused_devices",
  "refused_shared_namespace",
  "refused_relaxed_security",
  "refused_volumes_from",
  "refused_custom_runtime",
  "refused_legacy_links",
  "refused_host_user_namespace",
  "refused_docker_socket",
  "refused_bind_outside_data_roots",
  "refused_mount_type",
  "refused_multiple_data_mounts",
  "refused_image_volume",
  "refused_environment_entry",
  "refused_multiline_environment",
  "refused_environment_value_size",
  "refused_environment_count",
  "refused_unknown_secret_env",
  "refused_http_port_ambiguous",
  "refused_http_service_ambiguous",
  "refused_name",
  "refused_existing_application_volume",
  "refused_data_unmeasurable",
  "refused_data_disk_space",
  "refused_contract",
  "refused_review",
  "refused_review_outbound",
]);

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
  if (ROW_CODES.has(row.code)) {
    return tr(`ui.stackIt.row.${row.code}`, values);
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

const OUTCOMES: Record<string, string> = {
  rolled_back: "rolledBack",
  rollback_failed: "rollbackFailed",
  refused: "refused",
  unhealthy: "unhealthy",
};

const REASON_CODES: Record<string, string> = {
  application_stack_unavailable: "unavailable",
  application_adoption_unavailable: "unavailable",
  application_stacked: "alreadyStacked",
  application_stack_retention_ended: "retentionEnded",
  application_stack_name_unavailable: "refused",
  application_source_identity_unavailable: "refused",
  application_adoption_profile_unsupported: "refused",
  application_source_changed: "changed",
  application_intent_changed: "changed",
  service_locked: "locked",
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
    if (key) return tr(`ui.stackIt.error.${key}`);
    if (error.status === 409) return tr("ui.stackIt.error.changed");
    if (error.status === 503) return tr("ui.stackIt.error.unavailable");
    return tr("ui.stackIt.error.failed");
  }
  if (error instanceof StackItJobError) {
    const reason = error.reasonCode ? REASON_CODES[error.reasonCode] : undefined;
    if (reason) return tr(`ui.stackIt.error.${reason}`);
    const outcome = OUTCOMES[error.outcome];
    if (outcome) {
      const line = tr(`ui.stackIt.error.${outcome}`);
      return error.rowCode && error.outcome === "refused"
        ? `${line} ${stackItRowText({ code: error.rowCode })}`
        : line;
    }
  }
  return tr("ui.stackIt.error.failed");
}
