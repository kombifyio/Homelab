<script lang="ts">
  import { tick, untrack } from "svelte";
  import { ChevronLeft, ChevronRight, Plus } from "@lucide/svelte";
  import {
    UseCaseCardCompact,
    type SetupHardwareHints,
  } from "@kombiverselabs/ui/usecase";
  import type { CreationGoal } from "#lib/wizard/creationGoals.js";
  import { goalSummary, goalTitle } from "#lib/wizard/creationGoalText.js";
  import type { GoalConfigKey } from "#lib/wizard/index.js";
  import type { UseCaseCatalogView } from "#lib/api/useCaseCatalog.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { useCreationContext } from "#lib/creation/context.js";
  import type { GoalsSlice } from "#lib/creation/core/index.js";
  import CreationGoalCard from "./CreationGoalCard.svelte";
  import CapacityHint from "./CapacityHint.svelte";
  import type {
    CapacityHint as CapacityHintModel,
    CapacitySize,
  } from "#lib/wizard/capacity-view.js";
  import type {
    DetectedExistingApp,
    SmartHomeRecommendation,
  } from "#lib/api/unifier.js";

  /**
   * Step 1, second stage: the chosen use cases as a compact list, and beside
   * it the large card of one of them. The arrows walk through the choice; each
   * card starts on its recommendation, so forward without a click accepts the
   * defaults. A process that came for one use case shows only its card.
   */
  let {
    cards,
    config = $bindable(),
    runtimeCatalog,
    reviewMode = false,
    onGoalChange,
    onSettingChange,
    hardware = {},
    single,
    initial,
    onadd,
    ondisclose,
    capacityHint,
    onCapacitySize,
    onCapacityOmit,
    smartHomeRecommendation,
    detectedExistingApps = [],
    onAdoptExistingApp,
    onOverrideExistingApp,
  }: {
    /** The chosen use cases, in portfolio order. */
    cards: CreationGoal[];
    config: GoalsSlice;
    runtimeCatalog: Map<string, UseCaseCatalogView>;
    reviewMode?: boolean;
    onGoalChange: (goal: GoalConfigKey, value: boolean) => void;
    onSettingChange: (
      goal: GoalConfigKey,
      setting: string,
      value: string | boolean,
    ) => void;
    hardware?: SetupHardwareHints;
    /** The one use case the person came for: its card alone, without a list. */
    single?: CreationGoal;
    /** The use case to open first (a link that handed over its answers). */
    initial?: string;
    /** Back to the choice, to add a use case. */
    onadd: () => void;
    ondisclose?: (event: { kind: "drawer"; id: string; open: boolean }) => void;
    capacityHint?: CapacityHintModel;
    onCapacitySize?: (size: CapacitySize) => void;
    onCapacityOmit?: (goal: string) => void;
    smartHomeRecommendation?: SmartHomeRecommendation;
    detectedExistingApps?: DetectedExistingApp[];
    onAdoptExistingApp?: (app: DetectedExistingApp) => void;
    onOverrideExistingApp?: (app: DetectedExistingApp) => void;
  } = $props();

  const appearance = useCreationContext().appearance;
  const shown = $derived(single ? [single] : cards);
  let currentId = $state(untrack(() => initial ?? shown[0]?.id));
  // A removed use case leaves the list; the stage keeps its place.
  let lastIndex = 0;
  const index = $derived.by(() => {
    const found = shown.findIndex((card) => card.id === currentId);
    return found >= 0 ? found : Math.min(lastIndex, shown.length - 1);
  });
  const current = $derived(shown[index]);
  let stage = $state<HTMLElement>();

  $effect(() => {
    lastIndex = index;
  });
  // The open card is the techiness signal the old drawer sent.
  $effect(() => {
    const id = current?.id;
    if (!id) return;
    untrack(() => ondisclose?.({ kind: "drawer", id, open: true }));
    return () =>
      untrack(() => ondisclose?.({ kind: "drawer", id, open: false }));
  });

  async function show(target: number) {
    const card = shown[target];
    if (!card) return;
    currentId = card.id;
    await tick();
    stage?.scrollIntoView({ block: "nearest", behavior: "smooth" });
  }
</script>

<header class="goals-heading">
  <div>
    <p class="eyebrow">{tr("wizard.preview.eyebrow")}</p>
    {#if single}
      <h2>{tr("wizard.focus.title", { useCase: goalTitle(single) })}</h2>
      <p class="intro">{tr("wizard.focus.subtitle")}</p>
    {:else}
      <h2>{tr("wizard.goals.configure.title")}</h2>
      <p class="intro">{tr("wizard.goals.configure.subtitle")}</p>
    {/if}
  </div>
</header>

<div
  class="configure"
  class:single={Boolean(single)}
  data-testid="creation-goals-configure"
>
  {#if !single}
    <aside class="chosen" aria-labelledby="chosen-title">
      <p id="chosen-title" class="chosen-title">
        {tr("wizard.goals.configure.list")}<span>{shown.length}</span>
      </p>
      <ul>
        {#each shown as card, position (card.id)}
          <li
            class:current={position === index}
            data-testid={`configure-item-${card.id}`}
          >
            <UseCaseCardCompact
              name={goalTitle(card)}
              description={goalSummary(card)}
              layout="row"
              art={card.id}
              dark={$appearance === "dark"}
              planState="included"
              selected={position === index}
              selectionLabel={`${tr("wizard.goals.remove")} ${goalTitle(card)}`}
              planLabel={tr("wizard.preview.selected")}
              onToggle={(next) => {
                if (card.question) onGoalChange(card.question.configKey, next);
              }}
            />
            <button
              type="button"
              class="row-open"
              aria-current={position === index ? "true" : undefined}
              aria-label={tr("wizard.goals.configure.open", {
                useCase: goalTitle(card),
              })}
              onclick={() => show(position)}
            ></button>
          </li>
        {/each}
      </ul>
      <button
        type="button"
        class="add"
        data-testid="configure-add"
        onclick={onadd}
      >
        <Plus size={16} aria-hidden="true" />{tr("wizard.goals.configure.add")}
      </button>
      {#if capacityHint}
        <CapacityHint
          hint={capacityHint}
          onsize={onCapacitySize}
          onomit={onCapacityOmit}
        />
      {/if}
    </aside>
  {/if}

  <section class="stage" bind:this={stage} aria-live="polite">
    {#if !single}
      <button
        type="button"
        class="arrow"
        data-testid="configure-previous"
        aria-label={tr("wizard.goals.configure.previous")}
        title={tr("wizard.goals.configure.previous")}
        disabled={index <= 0}
        onclick={() => show(index - 1)}
        ><ChevronLeft size={20} aria-hidden="true" /></button
      >
    {/if}
    {#if current}
      {#key current.id}
        <CreationGoalCard
          card={current}
          bind:config
          {runtimeCatalog}
          {reviewMode}
          {onGoalChange}
          {onSettingChange}
          {hardware}
          oncomplete={() => {
            if (index < shown.length - 1) void show(index + 1);
          }}
          {smartHomeRecommendation}
          {detectedExistingApps}
          {onAdoptExistingApp}
          {onOverrideExistingApp}
        />
      {/key}
    {/if}
    {#if !single}
      <button
        type="button"
        class="arrow"
        data-testid="configure-next"
        aria-label={tr("wizard.goals.configure.next")}
        title={tr("wizard.goals.configure.next")}
        disabled={index >= shown.length - 1}
        onclick={() => show(index + 1)}
        ><ChevronRight size={20} aria-hidden="true" /></button
      >
      <p class="position">
        {tr("wizard.goals.configure.position", {
          current: index + 1,
          total: shown.length,
        })}
      </p>
    {/if}
  </section>
  {#if single && capacityHint}
    <CapacityHint
      hint={capacityHint}
      onsize={onCapacitySize}
      onomit={onCapacityOmit}
    />
  {/if}
</div>

<style>
  .goals-heading {
    padding-block: 10px 8px;
  }
  .eyebrow {
    font-size: 10px;
    font-weight: 650;
    text-transform: uppercase;
    letter-spacing: 0.12em;
    color: var(--primary);
    margin: 0 0 6px;
  }
  h2 {
    font-size: clamp(25px, 2.4vw, 32px);
    font-weight: 600;
    letter-spacing: -0.035em;
    line-height: 1.15;
    margin: 0;
    text-wrap: balance;
  }
  .intro {
    max-width: 72ch;
    margin: 12px 0 0;
    font-size: 14px;
    line-height: 1.6;
    color: var(--muted-foreground);
  }

  /* The list on the left, the stage on the right; both keep their place
     while the person walks through the choice. */
  .configure {
    display: grid;
    grid-template-columns: minmax(250px, 300px) minmax(0, 1fr);
    gap: 20px;
    align-items: start;
  }
  .configure.single {
    grid-template-columns: minmax(0, 1fr);
  }
  .chosen {
    position: sticky;
    top: 16px;
    display: grid;
    gap: 10px;
  }
  .chosen-title {
    display: flex;
    align-items: center;
    gap: 8px;
    margin: 0;
    font-size: 10px;
    font-weight: 650;
    letter-spacing: 0.12em;
    text-transform: uppercase;
    color: var(--muted-foreground);
  }
  .chosen-title span {
    padding: 1px 8px;
    border: 1px solid var(--border);
    border-radius: 999px;
    letter-spacing: 0;
    font-variant-numeric: tabular-nums;
  }
  ul {
    display: grid;
    gap: 8px;
    margin: 0;
    padding: 0;
    list-style: none;
  }
  li {
    position: relative;
    min-width: 0;
  }
  /* The open layer lies under the row; the row lets clicks through to it
     (its shell isolates its own stacking) and only its Remove mark takes them. */
  .row-open {
    position: absolute;
    inset: 0;
    border-radius: var(--radius-lg, 12px);
    background: transparent;
    cursor: pointer;
  }
  .row-open:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }
  li :global(.kf-uc-enhanced) {
    position: relative;
    z-index: 1;
    pointer-events: none;
  }
  li :global(.kf-uc-select) {
    pointer-events: auto;
  }
  li:not(.current) :global(.kf-uc-enhanced) {
    opacity: 0.82;
  }
  .add {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    gap: 8px;
    padding: 10px 14px;
    border: 1px dashed var(--border);
    border-radius: var(--radius-lg, 12px);
    color: var(--muted-foreground);
    font-size: 13px;
    font-weight: 550;
    cursor: pointer;
  }
  .add:hover {
    color: var(--foreground);
    border-color: var(--primary);
  }
  .add:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }

  .stage {
    display: grid;
    grid-template-columns: auto minmax(0, 1fr) auto;
    align-items: center;
    gap: 10px;
    min-width: 0;
    scroll-margin-block: 16px;
  }
  .single .stage {
    grid-template-columns: minmax(0, 1fr);
  }
  .arrow {
    display: grid;
    place-items: center;
    width: 40px;
    height: 40px;
    border: 1px solid var(--border);
    border-radius: 50%;
    background: var(--card);
    color: var(--foreground);
    box-shadow: 0 1px 4px color-mix(in oklch, var(--foreground) 8%, transparent);
    cursor: pointer;
    transition:
      border-color 120ms ease,
      opacity 120ms ease;
  }
  .arrow:hover:not(:disabled) {
    border-color: var(--primary);
  }
  .arrow:disabled {
    opacity: 0.35;
    cursor: not-allowed;
  }
  .arrow:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }
  :global(html[dir="rtl"]) .arrow :global(svg) {
    transform: scaleX(-1);
  }
  .position {
    grid-column: 2;
    margin: 0;
    text-align: center;
    font-size: 12px;
    color: var(--muted-foreground);
    font-variant-numeric: tabular-nums;
  }

  button:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 4px;
  }

  /* Narrow: the list above, the arrows under the card. */
  @media (max-width: 900px) {
    .configure {
      grid-template-columns: minmax(0, 1fr);
    }
    .chosen {
      position: static;
    }
    .stage {
      grid-template-columns: auto 1fr auto;
    }
    .stage :global(.goal-card) {
      grid-column: 1 / -1;
      grid-row: 1;
    }
    .arrow {
      grid-row: 2;
    }
    .position {
      grid-row: 2;
    }
  }
</style>
