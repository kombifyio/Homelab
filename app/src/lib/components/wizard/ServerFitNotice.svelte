<script lang="ts">
  import { Button } from "@kombiverselabs/ui/primitives";
  import type { ServerFit, ServerFitRecovery } from "#lib/api/unifier.js";
  import type { WizardPreviewState } from "#lib/wizard/WizardPreviewController.js";
  import { formatNumber, tr } from "#lib/i18n.svelte.js";
  import { getEasyGoalQuestions } from "#lib/wizard/index.js";
  import { useCaseTitle } from "#lib/wizard/creationGoalText.js";

  export type ServerFitTarget =
    | { server_id: string }
    | { provider_id: string; offering_id: string }
    | { source: "guard" | "ssh" | "substrate" | "install-command" | "existing"; host?: string };

  let {
    state,
    target,
    onrecovery,
    onrecheck,
  }: {
    state?: WizardPreviewState;
    target?: ServerFitTarget;
    onrecovery?: (recovery: ServerFitRecovery) => void;
    onrecheck?: () => void;
  } = $props();

  // Previous or differently scoped results never describe the current choice.
  const fit = $derived.by<ServerFit | undefined>(() => {
    if (!target || state?.phase !== "resolved") return undefined;
    const result = state.result.server_fit;
    if (!result) return undefined;
    if ("server_id" in target)
      return result.server_id === target.server_id ? result : undefined;
    if ("provider_id" in target)
      return result.provider_id === target.provider_id &&
        result.offering_id === target.offering_id
        ? result
        : undefined;
    return result.capacity_origin !== "catalog_sku" ? result : undefined;
  });

  const status = $derived(
    !target
      ? "unknown"
      : state?.phase === "loading"
        ? "checking"
        : (fit?.status ?? "unknown"),
  );

  const originKey = $derived.by(() => {
    if (!fit) return "wizard.server.fit.estimate";
    if (fit.capacity_origin === "catalog_sku")
      return "wizard.server.fit.origin.catalogSku";
    if (fit.capacity_origin === "unmeasured")
      return "wizard.server.fit.origin.unmeasured";
    if (fit.inventory_source === "ssh")
      return "wizard.server.fit.origin.observedSsh";
    if (fit.inventory_source === "substrate")
      return "wizard.server.fit.origin.observedSubstrate";
    if (fit.evidence === "observed" || fit.capacity_origin === "observed_inventory")
      return "wizard.server.fit.origin.observedGuard";
    return "wizard.server.fit.estimate";
  });

  const pendingKeys: Record<string, string> = {
    inventory_stale: "wizard.server.fit.pending.inventory_stale",
    inventory_unavailable: "wizard.server.fit.pending.inventory_unavailable",
    host_offline: "wizard.server.fit.pending.host_offline",
    usage_unobserved: "wizard.server.fit.pending.usage_unobserved",
    host_unmeasured: "wizard.server.fit.pending.host_unmeasured",
    ssh_unmeasured: "wizard.server.fit.pending.ssh_unmeasured",
    substrate_unmeasured: "wizard.server.fit.pending.substrate_unmeasured",
    server_unavailable: "wizard.server.fit.pending.server_unavailable",
    provider_package_unavailable:
      "wizard.server.fit.pending.provider_package_unavailable",
  };

  const pendingMessage = $derived.by(() => {
    if (status !== "pending" || !fit) return undefined;
    const key = fit.pending_reason
      ? (pendingKeys[fit.pending_reason] ?? "wizard.server.fit.pending.default")
      : "wizard.server.fit.pending.default";
    return tr(key);
  });

  const questions = getEasyGoalQuestions();
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
    host_unmeasured: "observation",
    ssh_unmeasured: "observation",
    substrate_unmeasured: "observation",
    no_goals: "goals",
    provider_package_unavailable: "package",
    profile_unsupported: "profile",
    photo_backend_unavailable: "backend",
  };

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

  function fmt(value: number): string {
    return formatNumber(value, { maximumFractionDigits: 1 });
  }

  function recoveryLabel(recovery: ServerFitRecovery): string {
    switch (recovery.action) {
      case "switch_offering": {
        const offering = recovery.offering_name || recovery.offering_id || "";
        return recovery.offering_sku
          ? tr("wizard.server.fit.recovery.switchOffering", {
              offering,
              sku: recovery.offering_sku,
            })
          : tr("wizard.server.fit.recovery.switchOfferingNoSku", { offering });
      }
      case "switch_profile":
        return recovery.profile === "immich-lite"
          ? tr("wizard.server.fit.recovery.switchProfileLite")
          : recovery.profile === "ente-photos"
            ? tr("wizard.server.fit.recovery.switchProfileEnte")
            : tr("wizard.server.fit.recovery.switchProfileStandard");
      case "omit_use_case":
        return tr("wizard.server.fit.recovery.omitUseCase", {
          useCase: recovery.goal ? useCaseTitle(recovery.goal) : "",
        });
      case "refresh_inventory":
        return tr("wizard.server.fit.recovery.refreshInventory");
      default:
        return tr("wizard.server.fit.recovery.reviewSelection");
    }
  }

  function triggerRecovery(recovery: ServerFitRecovery) {
    if (recovery.action === "refresh_inventory" && onrecheck) {
      onrecheck();
      return;
    }
    onrecovery?.(recovery);
  }
</script>

<section
  class="fit"
  data-testid="server-hardware-fit"
  data-fit-status={status}
  data-fit-origin={fit?.capacity_origin ?? ""}
  aria-live="polite"
>
  <header class="fit-header">
    <strong>{tr(`wizard.server.fit.${status}`)}</strong>
    {#if fit && status !== "checking"}
      <span class="fit-origin">{tr(originKey)}</span>
    {/if}
  </header>

  {#if fit && status !== "checking"}
    {#if (fit.sku || fit.template) && fit.location}
      <p class="fit-package" data-testid="server-fit-package">
        {tr("wizard.server.fit.packageLine", {
          sku: fit.sku || fit.template || "",
          region: fit.location,
        })}
      </p>
    {/if}
    {#if fit.specs && (fit.specs.cpu_cores || fit.specs.memory_mb || fit.specs.disk_gb)}
      <p>
        {tr("wizard.server.fit.specs", {
          cpu: fit.specs.cpu_cores,
          memory: fit.specs.memory_mb / 1024,
          disk: fit.specs.disk_gb,
        })}
      </p>
    {/if}
    {#if fit.budget}
      <p data-testid="server-fit-budget">
        {tr("wizard.server.fit.budget", {
          cpu: fmt(fit.budget.total_cpu_cores),
          ram: fmt(fit.budget.total_ram_gib),
          disk: fmt(fit.budget.total_storage_gb),
          baseRam: fmt(fit.budget.base_ram_gib),
        })}
      </p>
      {#if fit.budget.external_data_mount && fit.budget.external_data_volume_gb}
        <p>
          {tr("wizard.server.fit.externalVolume", {
            mount: fit.budget.external_data_mount,
            size: fit.budget.external_data_volume_gb,
          })}
        </p>
      {/if}
    {/if}
    {#if pendingMessage}
      <p class="pending-note" data-testid="server-fit-pending-reason">
        {pendingMessage}
      </p>
    {/if}
    {#if fit.memory_peak?.status === "unknown" && status !== "pending"}
      <p>{tr("wizard.server.fit.peakUnknown")}</p>
    {:else if fit.memory_peak?.status === "available" && fit.memory_peak.warning}
      <p class="peak-warning" data-testid="server-memory-peak-warning">
        {tr("wizard.server.fit.peakWarning", {
          estimated: fit.memory_peak.estimated_gib,
          available: fit.memory_peak.available_gib,
        })}
        {tr("wizard.server.fit.peakAction")}
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
    {#if fit}
      <p class="scope-note">
        {tr(
          fit.scope === "whole_stack"
            ? "wizard.server.fit.wholeStackNote"
            : "wizard.server.fit.projected",
        )}
      </p>
    {/if}
    {#if fit?.recovery && status !== "fits" && (onrecovery || (fit.recovery.action === "refresh_inventory" && onrecheck))}
      {@const recovery = fit.recovery}
      <div class="fit-recovery" data-testid="server-fit-recovery">
        <Button
          size="sm"
          variant="secondary"
          onclick={() => triggerRecovery(recovery)}
        >
          {recoveryLabel(recovery)}
        </Button>
        <span class="retained-note">{tr("wizard.server.fit.choicesRetained")}</span>
      </div>
    {:else if status !== "fits"}
      <p>
        {tr(
          status === "does_not_fit"
            ? "wizard.server.fit.actionChange"
            : "wizard.server.fit.actionCheck",
        )}
      </p>
    {/if}
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
  .fit[data-fit-status="tight"] {
    border-color: var(--warning, var(--primary));
  }
  .fit-header {
    display: flex;
    flex-wrap: wrap;
    align-items: baseline;
    justify-content: space-between;
    gap: 6px 12px;
  }
  .fit-origin {
    font-size: 11px;
    color: var(--muted-foreground);
  }
  .fit-package {
    color: var(--foreground);
    font-weight: 500;
  }
  .fit .peak-warning,
  .fit .pending-note {
    color: var(--foreground);
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
  .scope-note {
    font-size: 12px;
  }
  .fit-recovery {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px 12px;
    margin-top: 12px;
  }
  .retained-note {
    font-size: 11px;
    color: var(--muted-foreground);
  }
</style>
