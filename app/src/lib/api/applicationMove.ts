/** Native move of one installed application between two servers of the same owner. */
import { fetchApi } from "./client";
import type { JobDetail } from "./jobs";

/** Exactly one application moves; Techstack never moves a whole use case. */
export type ApplicationMoveRequest = {
  workloads: [string];
  engine: "native";
  source_agent_id: string;
  destination_stack_id: string;
  destination_agent_id: string;
  owner_approved: true;
};
export interface ApplicationMoveAccepted {
  job_id: string;
  kit_deployment_id: string;
  operation: "workload_relocation";
  engine: "native";
  source_retained: true;
}
export type ApplicationMoveState =
  | "moved"
  | "aborted"
  | "destination_verification_required"
  | "preflight_failed"
  | "failed";
export interface ApplicationMoveOutcome {
  state: ApplicationMoveState;
  workloads: string[];
  transfer_id?: string;
  source_retained: boolean;
  source_state?: string;
  readback_digest?: string;
  portable_configuration_digest?: string;
}
const MOVE_STATES: readonly ApplicationMoveState[] = [
  "moved",
  "aborted",
  "destination_verification_required",
  "preflight_failed",
  "failed",
];
// Terminal job states that read like one shown state: the applications run on
// the other server while this one still holds them stopped with their data,
// or this server keeps them stopped with their data until a node confirms.
const SHOWN_AS: Readonly<Record<string, ApplicationMoveState>> = {
  moved_source_retirement_pending: "moved",
  source_verification_required: "destination_verification_required",
  source_resume_required: "destination_verification_required",
};

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}
export async function startApplicationMove(options: {
  sourceStackId: string;
  request: ApplicationMoveRequest;
  idempotencyKey: string;
}): Promise<ApplicationMoveAccepted> {
  const response = await fetchApi<unknown>(
    `/api/v1/stacks/${encodeURIComponent(options.sourceStackId)}/relocations`,
    {
      method: "POST",
      headers: { "Idempotency-Key": options.idempotencyKey },
      body: JSON.stringify(options.request),
      timeoutMs: 60_000,
    },
  );
  const accepted = response.data;
  if (
    !isRecord(accepted) ||
    typeof accepted.job_id !== "string" ||
    !accepted.job_id ||
    accepted.kit_deployment_id !== options.sourceStackId ||
    accepted.operation !== "workload_relocation" ||
    accepted.engine !== "native" ||
    accepted.source_retained !== true
  ) {
    throw new Error(
      "The move acceptance does not match this StackKit. Retry the same attempt.",
    );
  }
  return {
    job_id: accepted.job_id,
    kit_deployment_id: options.sourceStackId,
    operation: "workload_relocation",
    engine: "native",
    source_retained: true,
  };
}

/** The native move outcome a finished relocation job records, if any. */
export function applicationMoveOutcome(
  job: JobDetail | undefined,
): ApplicationMoveOutcome | null {
  let result: unknown = job?.result;
  if (typeof result === "string") {
    try {
      result = JSON.parse(result);
    } catch {
      return null;
    }
  }
  const outcome = isRecord(result) ? result.workload_relocation : undefined;
  const state =
    isRecord(outcome) && typeof outcome.state === "string"
      ? (SHOWN_AS[outcome.state] ?? outcome.state)
      : undefined;
  if (
    !isRecord(outcome) ||
    outcome.engine !== "native" ||
    !MOVE_STATES.includes(state as ApplicationMoveState)
  ) {
    return null;
  }
  const readback = isRecord(outcome.destination_readback)
    ? outcome.destination_readback
    : {};
  const text = (value: unknown) =>
    typeof value === "string" && value ? value : undefined;
  return {
    state: state as ApplicationMoveState,
    workloads: Array.isArray(outcome.workloads)
      ? outcome.workloads.filter(
          (ref): ref is string => typeof ref === "string",
        )
      : [],
    transfer_id: text(outcome.transfer_id),
    source_retained: outcome.source_retained === true,
    source_state: text(outcome.source_state),
    readback_digest: text(readback.readback_digest),
    portable_configuration_digest: text(
      readback.portable_configuration_digest,
    ),
  };
}
