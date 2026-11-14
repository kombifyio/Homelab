<script lang="ts">
  import type { Snippet } from "svelte";
  import { Circle, Rows3 } from "@lucide/svelte";
  import {
    UseCaseCardCompact,
    UseCaseOrb,
  } from "@kombiverselabs/ui/usecase";
  import type {
    CreationGoal,
    CreationPresentation,
  } from "#lib/wizard/creationGoals.js";
  import { goalSummary, goalTitle } from "#lib/wizard/creationGoalText.js";
  import type { GoalConfigKey } from "#lib/wizard/index.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { useCreationContext } from "#lib/creation/context.js";
  import type { GoalsSlice } from "#lib/creation/core/index.js";

  /**
   * Step 1, first stage: the choice. Every use case is one small, fixed
   * element with its name and a short line of what it means, as a round orb
   * or a compact row. Choosing changes only its look, so nothing moves; the
   * setup of the chosen ones is the next stage.
   */
  let {
    cards,
    config,
    presentation = $bindable(),
    reviewMode = false,
    onGoalChange,
    templates,
    ring,
  }: {
    /** Available use cases first, then the ones coming soon. */
    cards: CreationGoal[];
    config: GoalsSlice;
    presentation: CreationPresentation;
    reviewMode?: boolean;
    onGoalChange: (goal: GoalConfigKey, value: boolean) => void;
    /** The template row above the use cases. */
    templates?: Snippet;
    /** A chosen use case held by profiles that are on: their colours. */
    ring?: (
      card: CreationGoal,
    ) => { gradient: string; stripe: string; label: string } | undefined;
  } = $props();

  const appearance = useCreationContext().appearance;
  const selectedCount = $derived(cards.filter(isSelected).length);

  function isSelected(card: CreationGoal) {
    return !!card.question && config.goals?.[card.question.configKey] === true;
  }
  function planState(card: CreationGoal) {
    if (card.availability !== "available") return "unavailable" as const;
    return isSelected(card) ? ("included" as const) : ("available" as const);
  }
  function toggle(card: CreationGoal, next: boolean) {
    if (card.question && card.availability === "available")
      onGoalChange(card.question.configKey, next);
  }
  function selectionLabel(card: CreationGoal) {
    return `${tr(isSelected(card) ? "wizard.goals.remove" : "wizard.goals.add")} ${goalTitle(card)}`;
  }
  function testId(card: CreationGoal) {
    if (card.availability !== "available") return `preview-coming-${card.id}`;
    return reviewMode ? `preview-goal-${card.id}` : card.question?.testId;
  }
  function summary(card: CreationGoal) {
    return card.availability === "available"
      ? goalSummary(card)
      : tr("wizard.preview.comingSoon");
  }
</script>

<header class="goals-heading">
  <div>
    <p class="eyebrow">{tr("wizard.preview.eyebrow")}</p>
    <h2>{tr("wizard.preview.title")}</h2>
    <p class="intro">{tr("wizard.preview.subtitle")}</p>
  </div>
  <div class="heading-side">
    <span class="selection-count" role="status" aria-live="polite">
      <strong>{selectedCount}</strong>
      {tr("wizard.preview.selectionCount")}
    </span>
    <div
      class="presentation-switch"
      role="group"
      aria-label={tr("wizard.goals.presentation.label")}
    >
      <button
        type="button"
        aria-pressed={presentation === "orb"}
        aria-label={tr("wizard.goals.presentation.orb")}
        title={tr("wizard.goals.presentation.orb")}
        data-testid="creation-presentation-orb"
        onclick={() => (presentation = "orb")}
        ><Circle size={15} aria-hidden="true" /></button
      >
      <button
        type="button"
        aria-pressed={presentation === "compact"}
        aria-label={tr("wizard.goals.presentation.compact")}
        title={tr("wizard.goals.presentation.compact")}
        data-testid="creation-presentation-compact"
        onclick={() => (presentation = "compact")}
        ><Rows3 size={15} aria-hidden="true" /></button
      >
    </div>
  </div>
</header>

{@render templates?.()}

<div
  class="pick-grid"
  data-presentation={presentation}
  data-testid="creation-goals-pick"
>
  {#each cards as card, index (card.id)}
    {#if presentation === "orb"}
      <!-- The wrapper carries the card's test id and plan, like the compact
           row's shell, so a journey reads both presentations alike. -->
      {@const profileRing = ring?.(card)}
      <div
        class="pick-orb"
        data-testid={testId(card)}
        data-plan={planState(card)}
        data-profiles={profileRing ? "true" : undefined}
        style:--profile-ring={profileRing?.gradient}
        title={profileRing?.label}
      >
        <UseCaseOrb
          name={goalTitle(card)}
          summary={summary(card)}
          art={card.id}
          dark={$appearance === "dark"}
          planState={planState(card)}
          unavailableReason={tr("wizard.preview.comingSoonHint")}
          priority={index < 8}
          aria-label={card.availability === "available"
            ? selectionLabel(card)
            : `${goalTitle(card)} · ${tr("wizard.preview.comingSoon")}`}
          onToggle={(next) => toggle(card, next)}
        />
      </div>
    {:else}
      <!-- Test id and plan sit on the row, which also holds the click layer. -->
      {@const profileRing = ring?.(card)}
      <div
        class="pick-row"
        data-testid={testId(card)}
        data-plan={planState(card)}
        data-profiles={profileRing ? "true" : undefined}
        style:--profile-ring={profileRing?.stripe}
        title={profileRing?.label}
      >
        <UseCaseCardCompact
          name={goalTitle(card)}
          description={summary(card)}
          layout="row"
          art={card.id}
          dark={$appearance === "dark"}
          planState={planState(card)}
          unavailableReason={tr("wizard.preview.comingSoonHint")}
          selectable={card.availability === "available"}
          selectionLabel={selectionLabel(card)}
          planLabel={tr(
            card.availability !== "available"
              ? "wizard.preview.comingSoon"
              : isSelected(card)
                ? "wizard.preview.selected"
                : "wizard.preview.optional",
          )}
          onToggle={(next) => toggle(card, next)}
        />
        {#if card.availability === "available"}
          <!-- The whole row chooses; the "+" stays the visible state above it. -->
          <button
            type="button"
            class="row-select"
            tabindex="-1"
            aria-hidden="true"
            onclick={() => toggle(card, !isSelected(card))}
          ></button>
        {/if}
      </div>
    {/if}
  {/each}
</div>

<style>
  .goals-heading {
    display: flex;
    justify-content: space-between;
    align-items: flex-end;
    gap: 24px;
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
  .heading-side {
    display: flex;
    align-items: center;
    gap: 14px;
    margin-bottom: 3px;
  }
  .selection-count {
    color: var(--muted-foreground);
    font-size: 12px;
    font-variant-numeric: tabular-nums;
    white-space: nowrap;
  }
  .selection-count strong {
    color: var(--foreground);
  }
  .presentation-switch {
    display: flex;
    gap: 2px;
    padding: 3px;
    background: var(--muted);
    border: 1px solid var(--border);
    border-radius: 999px;
  }
  .presentation-switch button {
    display: grid;
    place-items: center;
    width: 30px;
    height: 26px;
    border-radius: 999px;
    color: var(--muted-foreground);
    cursor: pointer;
  }
  .presentation-switch button[aria-pressed="true"] {
    color: var(--foreground);
    background: var(--card);
    box-shadow: 0 1px 3px color-mix(in oklch, var(--foreground) 8%, transparent);
  }
  .presentation-switch button:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 2px;
  }

  /* Orbs: one calm, fixed grid; six to a row on a desktop. */
  .pick-grid[data-presentation="orb"] {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(132px, 1fr));
    gap: 18px 8px;
    padding: 8px 0 4px;
  }
  .pick-orb {
    display: flex;
    justify-content: center;
    min-width: 0;
  }
  /* A chosen use case that profiles hold wears their colours as its ring, in
     equal parts, in place of the single primary ring. */
  .pick-orb[data-profiles] :global(.kf-uc-orb[aria-pressed="true"] .kf-uc-orb-disc) {
    box-shadow: 0 6px 16px color-mix(in oklch, var(--foreground) 14%, transparent);
  }
  .pick-orb[data-profiles] :global(.kf-uc-orb-disc::before) {
    content: "";
    position: absolute;
    inset: -6px;
    border-radius: 50%;
    background: var(--profile-ring);
    -webkit-mask: radial-gradient(
      farthest-side,
      transparent calc(100% - 3px),
      #000 calc(100% - 3px)
    );
    mask: radial-gradient(
      farthest-side,
      transparent calc(100% - 3px),
      #000 calc(100% - 3px)
    );
    pointer-events: none;
  }

  /* Compact rows: the name-first row of the use-case module. */
  .pick-grid[data-presentation="compact"] {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(min(100%, 300px), 1fr));
    gap: 10px;
  }
  .pick-row {
    position: relative;
    min-width: 0;
  }
  /* The compact row shows the same profile colours as a stripe at its start. */
  .pick-row[data-profiles]::before {
    content: "";
    position: absolute;
    inset-block: 6px;
    inset-inline-start: -5px;
    z-index: 2;
    width: 3px;
    border-radius: 2px;
    background: var(--profile-ring);
    pointer-events: none;
  }
  /* The click layer lies under the card. The card lets clicks through to it
     (its shell isolates its own stacking), and only its mark takes them. */
  .row-select {
    position: absolute;
    inset: 0;
    border-radius: var(--radius-lg, 12px);
    background: transparent;
    cursor: pointer;
  }
  .pick-row :global(.kf-uc-enhanced) {
    position: relative;
    z-index: 1;
    pointer-events: none;
  }
  .pick-row :global(.kf-uc-select) {
    pointer-events: auto;
  }

  /* Hosted in another product the choice fits one screen: a tighter heading
     and smaller orbs, the same elements in the same places. */
  :global([data-creation-hosted]) .goals-heading {
    padding-block: 0 4px;
  }
  :global([data-creation-hosted]) h2 {
    font-size: clamp(21px, 1.9vw, 26px);
  }
  :global([data-creation-hosted]) .intro {
    margin-top: 6px;
    font-size: 13px;
    line-height: 1.45;
  }
  /* A short frame (a laptop with the host's bars) keeps the heading only. */
  @media (max-height: 680px) {
    :global([data-creation-hosted]) .intro {
      display: none;
    }
  }
  :global([data-creation-hosted]) .pick-grid[data-presentation="orb"] {
    grid-template-columns: repeat(auto-fill, minmax(112px, 1fr));
    gap: 6px 6px;
    padding-top: 2px;
  }
  :global([data-creation-hosted]) .pick-orb :global(.kf-uc-orb) {
    --kf-uc-orb-size: 68px;
    padding-block: 4px 2px;
  }
  @media (max-height: 620px) {
    :global([data-creation-hosted]) .pick-orb :global(.kf-uc-orb) {
      --kf-uc-orb-size: 56px;
    }
  }
  @media (max-width: 640px) {
    .goals-heading {
      flex-wrap: wrap;
      gap: 8px;
    }
    .goals-heading > div:first-child {
      flex: 1 0 100%;
    }
    .heading-side {
      margin-inline-start: auto;
    }
    .pick-grid[data-presentation="orb"] {
      grid-template-columns: repeat(auto-fill, minmax(104px, 1fr));
    }
  }
</style>
