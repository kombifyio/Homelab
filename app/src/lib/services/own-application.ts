/**
 * Operation of an app StackKits runs as an own application (adopted with
 * Stack it): status, logs, backup, and the reviewed mutations. Every mutation
 * is a dry run first; the owner approves exactly that review, and Homelab
 * issues the own-application.lifecycle capability for it. A tag update is
 * never checked by kombify and says so. Messages come from structured
 * outcome and reason codes only.
 */
import {
  executeOwnApplicationOperation,
  readOwnApplicationOperation,
  startOwnApplicationOperation,
  type CanonicalService,
  type OwnApplicationAction,
  type OwnApplicationAnswer,
  type OwnApplicationJob,
  type OwnApplicationOperationRequest,
} from "../api/services";
import { ApiRequestError } from "../api/client";
import { getJob } from "../api/jobs";
import {
  confirmInApp,
  noticeInApp,
  promptInApp,
} from "../dialogs/in-app-dialog";
import { tr } from "../i18n.svelte";
import { byteSize } from "./stack-it-plan";

/** Own-application actions a card or the detail page offers. */
export function ownApplicationActions(
  service: Pick<CanonicalService, "own_application">,
): OwnApplicationAction[] {
  return service.own_application?.actions ?? [];
}

export function ownApplicationGrants(
  service: Pick<CanonicalService, "own_application">,
  action: OwnApplicationAction,
): boolean {
  return ownApplicationActions(service).includes(action);
}

const KNOWN_REASONS = new Set([
  "tag_not_versioned",
  "variant_change",
  "not_newer",
  "resolve_failed",
  "tag_moved",
  "checkpoint_failed",
  "checkpoint_space_insufficient",
  "checkpoint_unrestored",
  "checkpoint_in_flight",
  "project_unsupported",
  "operation_pending",
  "application_not_found",
  "verify_failed",
  "action_refused",
  "service_locked",
  "own_application_review_mismatch",
  "node_outcome_unknown",
]);

/** One localized line for a refused or failed step, by reason code. */
export function ownApplicationReason(code?: string): string {
  if (!code) return tr("ui.ownApp.reason.other");
  if (code === "own_application_review_expired") {
    return tr("ui.ownApp.reason.own_application_review_mismatch");
  }
  if (code.startsWith("advanced_capability_") || code === "own_application_capability_unavailable") {
    return tr("ui.ownApp.reason.capability");
  }
  if (code.startsWith("own_application_") && code.endsWith("_unavailable")) {
    const key = `ui.ownApp.unavailable.${code}`;
    const text = tr(key);
    if (text !== key) return text;
  }
  return KNOWN_REASONS.has(code)
    ? tr(`ui.ownApp.reason.${code}`)
    : tr("ui.ownApp.reason.other");
}

function requestError(error: unknown): string {
  if (error instanceof ApiRequestError) {
    const details = error.details as { reason_code?: unknown } | undefined;
    if (typeof details?.reason_code === "string") {
      return ownApplicationReason(details.reason_code);
    }
    if (error.status === 409) return tr("ui.ownApp.reason.own_application_review_mismatch");
  }
  return tr("ui.ownApp.reason.other");
}

async function settle(service: CanonicalService, jobId: string): Promise<OwnApplicationJob> {
  for (;;) {
    const job = await getJob(jobId);
    if (!["pending", "queued", "running", "waiting"].includes(job.state)) break;
    await new Promise((resolve) => setTimeout(resolve, 1500));
  }
  return readOwnApplicationOperation(service.id, jobId);
}

async function run(
  service: CanonicalService,
  request: OwnApplicationOperationRequest,
): Promise<OwnApplicationJob> {
  const started = await startOwnApplicationOperation(service, request, crypto.randomUUID());
  return settle(service, started.job_id || "");
}

async function execute(service: CanonicalService, review: OwnApplicationJob): Promise<OwnApplicationJob> {
  const started = await executeOwnApplicationOperation(
    service,
    review.job_id,
    review.result?.review_digest ?? "",
    crypto.randomUUID(),
  );
  return settle(service, started.job_id || "");
}

function appName(service: CanonicalService): string {
  return service.own_application?.name || service.name;
}

/** The localized outcome of a finished execution, or its refusal. */
export function ownApplicationOutcome(
  service: CanonicalService,
  job: OwnApplicationJob,
  step: string,
): { ok: boolean; message: string } {
  const answer: OwnApplicationAnswer = job.result ?? {};
  const name = appName(service);
  switch (answer.outcome) {
    case "started":
    case "stopped":
    case "restarted":
    case "discarded":
    case "backed_up":
    case "observed":
      return { ok: true, message: tr(`ui.ownApp.outcome.${answer.outcome}`, { name }) };
    case "updated":
      return {
        ok: true,
        message: tr("ui.ownApp.outcome.updated", {
          name,
          to: answer.target?.version || answer.target?.image || "",
        }),
      };
    case "rolled_back":
      return step === "update"
        ? { ok: false, message: tr("ui.ownApp.outcome.update_rolled_back") }
        : { ok: true, message: tr("ui.ownApp.outcome.rolled_back", { name }) };
    case "rollback_failed":
      return { ok: false, message: tr("ui.ownApp.outcome.rollback_failed") };
    case "unresolved":
      return { ok: false, message: tr("ui.ownApp.outcome.unresolved") };
    default:
      return { ok: false, message: ownApplicationReason(answer.reason_code) };
  }
}

async function reviewAndApprove(
  service: CanonicalService,
  request: OwnApplicationOperationRequest,
  confirm: (review: OwnApplicationAnswer) => Promise<boolean>,
  step: string,
): Promise<string | null> {
  const review = await run(service, request);
  if (review.result?.outcome !== "reviewed" || !review.result.review_digest) {
    return ownApplicationReason(review.result?.reason_code);
  }
  if (!(await confirm(review.result))) return null;
  const done = await execute(service, review);
  const outcome = ownApplicationOutcome(service, done, step);
  await noticeInApp({ title: appName(service), message: outcome.message });
  return outcome.ok ? null : outcome.message;
}

function actionTitle(action: string, name: string): string {
  const key = `ui.services.action.${action}`;
  const label = tr(key);
  return tr("ui.ownApp.confirm.title", { action: label === key ? action : label, name });
}

/**
 * Start, stop, restart or roll back the image: review, one approval of exactly
 * that review, execution. Returns a failure message or null.
 */
export async function runOwnApplicationPower(
  service: CanonicalService,
  action: "start" | "stop" | "restart" | "rollback",
): Promise<string | null> {
  const name = appName(service);
  try {
    return await reviewAndApprove(
      service,
      { operation: "review", action },
      () =>
        confirmInApp({
          title: actionTitle(action, name),
          message: tr(`ui.ownApp.confirm.${action}`, { name }),
          confirmText: tr("ui.ownApp.approve"),
          tone: action === "stop" || action === "rollback" ? "warning" : "primary",
        }),
      action,
    );
  } catch (error) {
    return requestError(error);
  }
}

/**
 * Update to a newer version tag of the app's own image. kombify never checks
 * an own application's image: the approval says so. A running version
 * kombify cannot compare (current_version_unknown) or another major version
 * (major_change) is reviewed again with allow_major only when the owner asks.
 */
export async function runOwnApplicationUpdate(service: CanonicalService): Promise<string | null> {
  const name = appName(service);
  const image = service.own_application?.status?.image || "";
  try {
    const tag = (
      await promptInApp({
        title: tr("ui.ownApp.update.title", { name }),
        message: tr("ui.ownApp.update.prompt", { name, image: image || "?" }),
        inputLabel: tr("ui.ownApp.update.tag"),
      })
    )?.trim();
    if (!tag) return null;
    let request: OwnApplicationOperationRequest = { operation: "update_review", image_tag: tag };
    let review = await run(service, request);
    const code = review.result?.reason_code;
    if (review.result?.outcome === "refused" && (code === "current_version_unknown" || code === "major_change")) {
      const allow = await confirmInApp({
        title: tr("ui.ownApp.update.title", { name }),
        message: tr("ui.ownApp.update.allowMajor", { name, tag }),
        confirmText: tr("ui.ownApp.update.allowMajorApprove"),
        tone: "warning",
      });
      if (!allow) return null;
      request = { ...request, allow_major: true };
      review = await run(service, request);
    }
    const answer = review.result;
    if (answer?.outcome !== "reviewed" || !answer.review_digest) {
      return ownApplicationReason(answer?.reason_code);
    }
    const lines = [
      tr("ui.ownApp.update.confirm", {
        name,
        from: answer.prior?.version || answer.prior?.image || image || "?",
        to: answer.target?.version || tag,
      }),
    ];
    if (answer.replaces_checkpoint) {
      lines.push(tr("ui.ownApp.update.replaces", { size: byteSize(answer.replaces_checkpoint.retained_bytes) }));
    }
    const approved = await confirmInApp({
      title: tr("ui.ownApp.update.title", { name }),
      message: lines.join("\n"),
      confirmText: tr("ui.ownApp.update.approve"),
      tone: "warning",
    });
    if (!approved) return null;
    const done = await execute(service, review);
    const outcome = ownApplicationOutcome(service, done, "update");
    await noticeInApp({ title: name, message: outcome.message });
    return outcome.ok ? null : outcome.message;
  } catch (error) {
    return requestError(error);
  }
}

/** Restore or discard the data checkpoint of the latest tag update. */
export async function runOwnApplicationCheckpoint(
  service: CanonicalService,
  mode: "rollback" | "discard",
): Promise<string | null> {
  const checkpoint = service.own_application?.status?.checkpoint;
  if (!checkpoint) return null;
  const name = appName(service);
  const size = byteSize(checkpoint.retained_bytes);
  try {
    return await reviewAndApprove(
      service,
      {
        operation: mode === "discard" ? "checkpoint_discard_review" : "checkpoint_rollback_review",
        checkpoint: checkpoint.operation_id,
      },
      () =>
        confirmInApp({
          title: mode === "discard" ? tr("ui.ownApp.discard.title") : tr("ui.ownApp.restore.title"),
          message:
            mode === "discard"
              ? tr("ui.ownApp.discard.confirm", { name, size })
              : tr("ui.ownApp.restore.confirm", { name, size }),
          confirmText: mode === "discard" ? tr("ui.ownApp.discard") : tr("ui.ownApp.restoreData"),
          tone: mode === "discard" ? "danger" : "warning",
        }),
      mode === "discard" ? "checkpoint-discard" : "checkpoint-rollback",
    );
  } catch (error) {
    return requestError(error);
  }
}

/** Back up the node now (every own application volume) after approval. */
export async function runOwnApplicationBackup(service: CanonicalService): Promise<string | null> {
  try {
    const approved = await confirmInApp({
      title: tr("ui.ownApp.backup.title"),
      message: tr("ui.ownApp.backup.confirm"),
      confirmText: tr("ui.ownApp.backup"),
    });
    if (!approved) return null;
    const done = await run(service, { operation: "backup", owner_approved: true });
    const outcome = ownApplicationOutcome(service, done, "backup");
    await noticeInApp({ title: appName(service), message: outcome.message });
    return outcome.ok ? null : outcome.message;
  } catch (error) {
    return requestError(error);
  }
}

/** Read the signed status (projected onto the row for the cards). */
export async function refreshOwnApplicationStatus(service: CanonicalService): Promise<string | null> {
  try {
    const done = await run(service, { operation: "status" });
    const outcome = ownApplicationOutcome(service, done, "status");
    return outcome.ok || done.result?.outcome === "observation_unavailable" ? null : outcome.message;
  } catch (error) {
    return requestError(error);
  }
}

/** The redacted log tail of every container of the app. */
export async function readOwnApplicationLogs(
  service: CanonicalService,
): Promise<{ text: string } | { error: string }> {
  try {
    const done = await run(service, { operation: "logs" });
    if (done.result?.outcome !== "observed") {
      return { error: ownApplicationReason(done.result?.reason_code) };
    }
    return { text: done.result.logs ?? "" };
  } catch (error) {
    return { error: requestError(error) };
  }
}

/** The health of the app from its last status observation. */
export function ownApplicationHealth(service: Pick<CanonicalService, "own_application">): string {
  const status = service.own_application?.status;
  if (!status?.observed || !status.containers?.length) return "unknown";
  if (status.containers.some((c) => c.health === "unhealthy")) return "unhealthy";
  if (status.containers.some((c) => c.state !== "running")) return "stopped";
  if (status.containers.some((c) => c.health === "starting")) return "starting";
  if (status.containers.every((c) => c.health === "healthy")) return "healthy";
  return "running";
}
