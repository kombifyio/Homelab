<script lang="ts">
  import type { WizardPreviewState } from "#lib/wizard/WizardPreviewController.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { getEasyGoalQuestions } from "#lib/wizard/index.js";

  let {
    state,
    target,
  }: {
    state?: WizardPreviewState;
    target?:
      { server_id: string } | { provider_id: string; offering_id: string };
  } = $props();
  // Previous or differently scoped results never describe the current choice.
  const fit = $derived.by(() => {
    if (!target || state?.phase !== "resolved") return undefined;
    const result = state.result.server_fit;
    if (!result) return undefined;
    if ("server_id" in target)
      return result.server_id === target.server_id ? result : undefined;
    return result.provider_id === target.provider_id &&
      result.offering_id === target.offering_id
      ? result
      : undefined;
  });
  const status = $derived(
    !target
      ? "unknown"
      : state?.phase === "loading"
        ? "checking"
        : (fit?.status ?? "unknown"),
  );
  const questions = getEasyGoalQuestions();
  const reasons = $derived.by(() => {
    const facts = fit
      ? [
          ...(fit.deficits ?? []).map((reason) => ({
            ...reason,
            kind: "deficit",
          })),
          ...(fit.unknowns ?? []).map((reason) => ({
            ...reason,
            kind: "unconfirmed",
          })),
        ]
      : [];
    const seen: string[] = [];
    return facts.filter((reason) => {
      const key = JSON.stringify([
        reason.kind,
        reasonKeys[reason.code] ?? "other",
        reason.goal,
        reason.required,
        reason.available,
        reason.unit,
      ]);
      if (seen.includes(key)) return false;
      seen.push(key);
      return true;
    });
  });
  const reasonKeys: Record<string, string> = {
    cpu: "cpu",
    selection_cpu: "cpu",
    memory: "memory",
    selection_memory: "memory",
    storage: "storage",
    selection_storage: "storage",
    cpu_architecture: "architecture",
    cpu_microarchitecture: "architecture",
    cpu_reservation: "cpu",
    accelerator: "accelerator",
    accelerator_vendor: "accelerator",
    accelerator_driver: "accelerator",
    catalog_footprint_unavailable: "footprint",
    selected_services: "footprint",
    base_stack_capacity: "foundation",
    usage: "usage",
    server_unavailable: "observation",
    no_goals: "goals",
    provider_package_unavailable: "package",
  };
</script>

<section
  class="fit"
  data-testid="server-hardware-fit"
  data-fit-status={status}
  aria-live="polite"
>
  <strong>{tr(`wizard.server.fit.${status}`)}</strong>
  {#if fit && status !== "checking"}
    <p>
      {tr(
        fit.evidence === "observed"
          ? "wizard.server.fit.observed"
          : "wizard.server.fit.estimate",
      )}
    </p>
    {#if fit.specs && (fit.specs.cpu_cores || fit.specs.memory_mb || fit.specs.disk_gb)}
      <p>
        {tr("wizard.server.fit.specs", {
          cpu: fit.specs.cpu_cores,
          memory: fit.specs.memory_mb / 1024,
          disk: fit.specs.disk_gb,
        })}
      </p>
    {/if}
    {#if reasons.length}
      <ul>
        {#each reasons as reason (reason)}
          <li>
            {#if reason.goal && questions.find((question) => question.configKey === reason.goal)}
              <strong
                >{tr(
                  questions.find(
                    (question) => question.configKey === reason.goal,
                  )?.titleKey ?? "",
                )}:</strong
              >
            {/if}
            {tr(`wizard.server.fit.${reason.kind}`, {
              reason: tr(
                `wizard.server.fit.reason.${reasonKeys[reason.code] ?? "other"}`,
              ),
            })}
            {#if reason.required != null && reason.available != null}
              ({tr("wizard.server.fit.amounts", {
                required: reason.required,
                available: reason.available,
                unit: reason.unit ?? "",
              })})
            {/if}
          </li>
        {/each}
      </ul>
    {/if}
  {/if}
  {#if status !== "checking"}
    {#if fit}<p>{tr("wizard.server.fit.projected")}</p>{/if}
    {#if status !== "fits"}<p>
        {tr(
          status === "does_not_fit"
            ? "wizard.server.fit.actionChange"
            : "wizard.server.fit.actionCheck",
        )}
      </p>{/if}
  {/if}
</section>

<style>
  .fit {
    border: 1px solid var(--border);
    border-radius: var(--radius-panel, 18px);
    padding: 16px 18px;
    background: var(--card);
    font-size: 13px;
    line-height: 1.6;
  }
  .fit[data-fit-status="does_not_fit"] {
    border-color: var(--destructive);
  }
  .fit p,
  .fit ul {
    color: var(--muted-foreground);
    margin-top: 6px;
  }
  .fit ul {
    padding-inline-start: 20px;
    list-style: disc;
  }
</style>
