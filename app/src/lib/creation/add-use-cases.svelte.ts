/**
 * Submitting more use cases to an existing StackKit deployment: the extend run,
 * what a refusal means for the person, and the state a page shows afterwards.
 * The run accepts the recommended distribution that was shown and extends the
 * deployment it names, which can be another deployment of the homelab than the
 * one the page was opened for; if the homelab changed since, nothing is
 * changed and the updated recommendation is shown to be confirmed again.
 */
import { parseApiError } from "#lib/api/errors.js";
import type { WizardRunResponse } from "#lib/api/wizardRuns.js";
import type {
  LayoutRecommendationAcceptance,
  LayoutRecommendationView,
  LayoutRunTarget,
} from "#lib/api/layout.js";
import { WizardInputError, type StackConfig } from "#lib/wizard/index.js";
import { tr } from "#lib/i18n.svelte.js";
import { useCaseTitle } from "#lib/wizard/creationGoalText.js";
import { useCaseAccessBlockedGoals } from "./use-case-access";
import { CREATION_PROCESSES } from "./core/index";
import { emitCreationEvent } from "./host";
import { layoutRefusal } from "./layout-refusal";
import { creationScreenHref, submitCreationRun } from "./submit";

export interface AddUseCasesNotice {
  tone: "warning" | "error" | "info";
  title: string;
  body: string;
  details?: string;
}

export class AddUseCasesRun {
  readonly process = CREATION_PROCESSES["add-use-case"];
  submitting = $state(false);
  result = $state<WizardRunResponse | null>(null);
  notice = $state<AddUseCasesNotice | null>(null);
  /** A replacement a superseded run handed back; shown to be confirmed again. */
  replacement = $state<LayoutRecommendationView | undefined>();
  /** Changes when the shown recommendation must be asked for again. */
  refresh = $state(0);
  /** The deployment the last run extended. */
  target = $state<LayoutRunTarget | null>(null);

  async submit(
    deployment: LayoutRunTarget,
    config: StackConfig,
    layoutRecommendation?: LayoutRecommendationAcceptance,
    layoutTarget?: LayoutRunTarget,
  ): Promise<void> {
    this.submitting = true;
    this.notice = null;
    // The accepted recommendation names the deployment that carries it out.
    const target =
      layoutRecommendation && layoutTarget
        ? {
            stackId: layoutTarget.stackId,
            stackName:
              layoutTarget.stackName ||
              (layoutTarget.stackId === deployment.stackId
                ? deployment.stackName
                : ""),
          }
        : deployment;
    this.target = target;
    try {
      const { run } = await submitCreationRun(this.process, config, {
        ...target,
        layoutRecommendation,
      });
      this.result = run;
      // Saved without a rollout: nothing further will report to the host.
      if (run.state === "accepted")
        emitCreationEvent({
          event: "completed",
          runId: run.run_id,
          stackId: target.stackId,
          outcome: "accepted",
        });
    } catch (err) {
      this.notice = this.#noticeFor(err);
    } finally {
      this.submitting = false;
    }
  }

  /** The Creation Screen for a run that follows one provisioning job. */
  progressHref(stackId: string, name: string): string {
    const result = this.result;
    // The inline group view follows every incremental node operation.
    if (!result?.job_id || result.job_ids?.length) return "";
    return creationScreenHref(
      this.process,
      { ...result, kit_deployment_id: stackId },
      result.name || name,
    );
  }

  #noticeFor(err: unknown): AddUseCasesNotice {
    if (err instanceof WizardInputError) {
      return {
        tone: "warning",
        title: tr("layout.addUseCases.failed.title"),
        body: err.message,
      };
    }
    const accessBlocked = useCaseAccessBlockedGoals(err);
    if (accessBlocked.length > 0) return useCaseAccessBlockedNotice(accessBlocked);
    const refusal = layoutRefusal(err, "add-use-case");
    if (refusal) {
      this.replacement = refusal.replacement;
      if (refusal.refresh) this.refresh += 1;
      return refusal.notice;
    }
    return {
      tone: "error",
      title: tr("layout.addUseCases.failed.title"),
      body: tr("layout.addUseCases.failed.body"),
      details: parseApiError(err).message,
    };
  }
}

/**
 * The run saved the access decisions and set nothing up: said as it is, with
 * the use cases it concerns by name.
 */
export function useCaseAccessBlockedNotice(goals: string[]): AddUseCasesNotice {
  return {
    tone: "info",
    title: tr("layout.addUseCases.access.blocked.title"),
    body: tr("layout.addUseCases.access.blocked.body", {
      useCases: goals.map(useCaseTitle).join(", "),
    }),
  };
}
