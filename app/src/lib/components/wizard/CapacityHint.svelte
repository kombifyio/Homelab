<!--
  Capacity hint (Unifier capacity advice).

  One compact line beside the selection: how much of the chosen server's
  memory the selected use cases take, and when it is tight the way out, as
  inline choices: a larger size, or a use case to leave out. It is advice and
  never blocks: nothing here disables Next or Create, the person may continue
  as chosen and a use case that cannot be installed fails alone at rollout.
  Used on the Goals and the Server step with the same model (`CapacityHint`
  from `capacity-view.ts`); numbers are gigabytes, text is localized.
-->
<script lang="ts">
  import { MemoryStick } from "@lucide/svelte";
  import { Button } from "@kombiverselabs/ui/primitives";
  import { formatNumber, tr } from "#lib/i18n.svelte.js";
  import { useCaseTitle } from "#lib/wizard/creationGoalText.js";
  import type {
    CapacityHint as CapacityHintModel,
    CapacitySize,
  } from "#lib/wizard/capacity-view.js";

  interface Props {
    hint: CapacityHintModel;
    /** The person chose the larger size. */
    onsize?: (size: CapacitySize) => void;
    /** The person leaves a use case out. */
    onomit?: (goal: string) => void;
  }

  let { hint, onsize, onomit }: Props = $props();

  function gb(value: number): string {
    return formatNumber(value, { maximumFractionDigits: 1 });
  }

  const tight = $derived(hint.status === "tight");
  const estimate = $derived(hint.status === "estimate");
  const unknown = $derived(hint.status === "unknown");
  const numbers = $derived({
    need: gb(hint.needGb),
    have: gb(hint.haveGb),
    existing: gb(hint.existingHaveGb ?? 0),
  });
  const driverKeys: Record<string, string> = {
    photos_smart_search: "capacity.driver.photosSmartSearch",
    photos_lite: "capacity.driver.photosLite",
    photos_ente: "capacity.driver.photosEnte",
    files_paperless: "capacity.driver.filesPaperless",
    files_nextcloud: "capacity.driver.filesNextcloud",
    files_opencloud: "capacity.driver.filesOpencloud",
    media_transcoding: "capacity.driver.mediaTranscoding",
    media_library: "capacity.driver.mediaLibrary",
    ai_local_models: "capacity.driver.aiLocalModels",
    smart_home_haos: "capacity.driver.smartHomeHaos",
    marketplace_runtime: "capacity.driver.marketplaceRuntime",
    compute_profile_low: "capacity.driver.profileLow",
    compute_profile_high: "capacity.driver.profileHigh",
  };
  const driverLabels = $derived(
    (hint.drivers ?? [])
      .map((driver) => driverKeys[driver.code])
      .filter((key): key is string => Boolean(key))
      .map((key) => tr(key)),
  );
</script>

<div
  class="capacity-hint"
  class:tight
  data-testid="capacity-hint"
  data-capacity-status={hint.status}
  aria-live="polite"
>
  <MemoryStick size={15} aria-hidden="true" />
  <div class="line">
    {#if unknown}
      <span class="state">{tr("capacity.notAssessed")}</span>
    {:else if estimate}
      <span class="state"
        >{tr(
          hint.atLeast ? "capacity.estimateAtLeast" : "capacity.estimate",
          numbers,
        )}</span
      >
      {#if hint.existingHaveGb !== undefined && hint.existingHaveGb < hint.needGb}
        <span class="supplement"
          >{tr("capacity.existingShortfall", numbers)}</span
        >
      {/if}
    {:else}
      <span class="state"
        >{tr(
          tight
            ? hint.atLeast
              ? "capacity.tightAtLeast"
              : "capacity.tight"
            : "capacity.fits",
          numbers,
        )}</span
      >
      <div
        class="meter"
        role="meter"
        aria-label={tr("capacity.meter", numbers)}
        aria-valuemin={0}
        aria-valuemax={hint.haveGb}
        aria-valuenow={Math.min(hint.needGb, hint.haveGb)}
      >
        <span style:width={`${Math.round(hint.share * 100)}%`}></span>
      </div>
    {/if}
    {#if driverLabels.length > 0 && !unknown}
      <ul class="drivers" data-testid="capacity-drivers">
        {#each driverLabels as label (label)}
          <li>{label}</li>
        {/each}
      </ul>
    {/if}
  </div>
  {#if tight && ((hint.size && onsize) || (hint.omit.length > 0 && onomit))}
    <div class="choices">
      {#if hint.size && onsize}
        {@const size = hint.size}
        <Button size="sm" variant="secondary" onclick={() => onsize?.(size)}
          >{tr("capacity.useSize", { size: gb(size.gb) })}</Button
        >
      {/if}
      {#if onomit}
        {#each hint.omit as goal (goal)}
          <Button size="sm" variant="ghost" onclick={() => onomit?.(goal)}
            >{tr("capacity.leaveOut", { useCase: useCaseTitle(goal) })}</Button
          >
        {/each}
      {/if}
    </div>
  {/if}
</div>

<style>
  .capacity-hint {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 8px 14px;
    padding: 8px 2px;
    color: var(--muted-foreground);
    font-size: 12px;
  }
  .line {
    display: grid;
    gap: 6px;
    min-width: 180px;
    flex: 1 1 220px;
  }
  .state {
    color: var(--foreground);
  }
  .supplement {
    color: var(--muted-foreground);
    font-size: 11px;
    line-height: 1.45;
  }
  .meter {
    height: 3px;
    border-radius: 999px;
    background: var(--border);
    overflow: hidden;
  }
  .meter span {
    display: block;
    height: 100%;
    border-radius: inherit;
    background: var(--primary);
  }
  .tight :global(svg),
  .tight .meter span {
    color: var(--warning, var(--primary));
    background-color: var(--warning, var(--primary));
  }
  .tight :global(svg) {
    background-color: transparent;
  }
  .choices {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .drivers {
    margin: 2px 0 0;
    padding-inline-start: 16px;
    list-style: disc;
    font-size: 11px;
    line-height: 1.5;
    color: var(--muted-foreground);
  }
</style>
