<script lang="ts">
  import { onMount, tick } from "svelte";
  import type { SetupHardwareHints } from "@kombiverselabs/ui/usecase";
  import {
    creationGoals,
    supportsCreationChoice,
    type CreationGoal,
    type CreationPresentation,
    type CreationStage,
  } from "#lib/wizard/creationGoals.js";
  import {
    getEasyGoalQuestions,
    type GoalConfigKey,
    type BundleQuestionDefinition,
  } from "#lib/wizard/index.js";
  import type { UseCaseCatalogView } from "#lib/api/useCaseCatalog.js";
  import { tr } from "#lib/i18n.svelte.js";
  import { useCreationContext } from "#lib/creation/context.js";
  import type { GoalsSlice } from "#lib/creation/core/index.js";
  import CreationGoalsPick from "./CreationGoalsPick.svelte";
  import CreationGoalsConfigure from "./CreationGoalsConfigure.svelte";
  import CreationTemplates from "./CreationTemplates.svelte";
  import {
    CREATION_TEMPLATES,
    STANDARD_STARTS,
    goalTemplates,
    liveTemplates,
    templateHolds,
    ringGradient,
    templateSelection,
    toggleTemplate,
    type CreationTemplateId,
  } from "#lib/wizard/creationTemplates.js";

  /**
   * Step 1 in two stages (workspace KOMBIFY-DESIGN-SYSTEM-STANDARD.md §9):
   * the choice of use cases as small fixed elements, then the configuration
   * of the chosen ones, one large card at a time. The host owns the stage, so
   * its forward and back buttons move between them.
   */
  let {
    config = $bindable(),
    catalog,
    onGoalChange,
    onSettingChange,
    stage,
    onstagechange,
    reviewMode = false,
    questions = getEasyGoalQuestions(),
    runtimeCatalog = catalog,
    ondisclose,
    hardware = {},
    focus,
  }: {
    config: GoalsSlice;
    catalog: Map<string, UseCaseCatalogView>;
    onGoalChange: (goal: GoalConfigKey, value: boolean) => void;
    onSettingChange: (
      goal: GoalConfigKey,
      setting: string,
      value: string | boolean,
    ) => void;
    stage: CreationStage;
    onstagechange: (stage: CreationStage) => void;
    reviewMode?: boolean;
    questions?: BundleQuestionDefinition<GoalConfigKey>[];
    runtimeCatalog?: Map<string, UseCaseCatalogView>;
    ondisclose?: (event: {
      kind: "detail-level" | "drawer" | "group";
      id?: string;
      open: boolean;
    }) => void;
    /** Hardware Techstack already knows, for the guided-setup recommendation. */
    hardware?: SetupHardwareHints;
    /**
     * The one use case the person came for: only its card, opened. A use case
     * the catalog cannot offer leaves the whole choice.
     */
    focus?: string;
  } = $props();

  const PRESENTATION_KEY = "kombify.creation.presentation";
  const allCards = $derived(creationGoals(questions, catalog));
  const single = $derived(
    allCards.find(
      (card) => card.id === focus && card.availability === "available",
    ),
  );
  const available = $derived(
    allCards.filter((card) => card.availability === "available"),
  );
  const ordered = $derived([
    ...available,
    ...allCards.filter((card) => card.availability !== "available"),
  ]);
  const chosen = $derived(available.filter(isSelected));

  const creation = useCreationContext();
  const standardStart = $derived(
    creation.features.isEnabled("server_hardware_fit"),
  );

  // The standard starting profile and its added goals travel with the draft;
  // only explicitly selected use cases enter the creation run.
  let switchedOn = $state<CreationTemplateId[]>([]);
  const profiles = $derived(
    CREATION_TEMPLATES.map((template) => ({
      template,
      selection: templateSelection(template, allCards, standardStart),
    })).filter(({ selection }) => selection.goals.length > 0),
  );
  const selections = $derived(
    new Map(
      profiles.map(({ template, selection }) => [template.id, selection]),
    ),
  );
  const live = $derived(
    liveTemplates(
      standardStart
        ? Object.keys(config.creationProfiles ?? {}).filter(
            (id): id is CreationTemplateId =>
              CREATION_TEMPLATES.some((template) => template.id === id),
          )
        : switchedOn,
      selections,
      config.goals,
    ),
  );

  function toggleProfile(id: CreationTemplateId) {
    if (standardStart) {
      const selection = selections.get(id);
      if (!selection) return;
      config.creationProfiles ??= {};
      if (live.includes(id)) {
        const added = config.creationProfiles[id]?.added ?? [];
        delete config.creationProfiles[id];
        const held = new Set(
          live
            .filter((other) => other !== id)
            .flatMap((other) => selections.get(other)?.goals ?? []),
        );
        for (const goal of added) {
          const successor = live.find(
            (other) =>
              other !== id && templateHolds(selections.get(other), goal),
          );
          if (successor) config.creationProfiles[successor]?.added.push(goal);
          else if (!held.has(goal)) onGoalChange(goal, false);
        }
      } else {
        const added = selection.goals.filter((goal) => !config.goals?.[goal]);
        config.creationProfiles[id] = { added, skipped: selection.skipped };
        for (const goal of selection.goals) {
          // Never replace an existing choice or an explicit setting.
          if (!config.goals?.[goal]) {
            const card = allCards.find(
              (item) => item.question?.configKey === goal,
            );
            const values = config.useCaseSettings?.[goal] ?? {};
            const settings: Record<string, string> = {
              ...(goal === "photos" ? { backend: "immich-lite" } : {}),
              ...(goal === "smart-home"
                ? { "operating-form": "container" }
                : {}),
              ...(goal === "mail" ? { hosting: "existing" } : {}),
            };
            for (const [setting, value] of Object.entries(settings)) {
              if (
                values[setting] === undefined &&
                supportsCreationChoice(
                  runtimeCatalog.get(card?.id ?? ""),
                  setting,
                  value,
                )
              )
                onSettingChange(goal, setting, value);
            }
          }
          onGoalChange(goal, true);
        }
      }
      return;
    }
    const next = toggleTemplate(id, live, selections);
    switchedOn = next.on;
    for (const goal of next.select) onGoalChange(goal, true);
    for (const goal of next.deselect) onGoalChange(goal, false);
  }

  // A use case taken away by hand leaves the selection, and every profile
  // that held it is no longer on, also when the use case comes back.
  function changeGoal(goal: GoalConfigKey, value: boolean) {
    const previouslyLive = live;
    onGoalChange(goal, value);
    if (!value && standardStart && config.creationProfiles) {
      for (const id of previouslyLive) {
        if (templateHolds(selections.get(id), goal))
          delete config.creationProfiles[id];
      }
    }
    if (!value)
      switchedOn = switchedOn.filter(
        (id) => !templateHolds(selections.get(id), goal),
      );
  }

  /** The ring of a chosen use case: the colours of the profiles holding it. */
  function profileRing(card: CreationGoal) {
    if (!card.question || !isSelected(card)) return undefined;
    const holding = goalTemplates(card.question.configKey, live, selections);
    const colors = holding.map((template) => template.color);
    const gradient = ringGradient(colors);
    const stripe = ringGradient(colors, "stripe");
    if (!gradient || !stripe) return undefined;
    return {
      gradient,
      stripe,
      label: tr("wizard.templates.partOf", {
        profiles: holding.map((template) => tr(template.titleKey)).join(", "),
      }),
    };
  }
  let presentation = $state<CreationPresentation>(readPresentation());
  let initial = $state<string | undefined>();
  let root = $state<HTMLDivElement>();

  function isSelected(card: CreationGoal) {
    return !!card.question && config.goals?.[card.question.configKey] === true;
  }
  /** The view is a per-viewer convenience; without storage it is the orb. */
  function readPresentation(): CreationPresentation {
    try {
      return localStorage.getItem(PRESENTATION_KEY) === "compact"
        ? "compact"
        : "orb";
    } catch {
      return "orb";
    }
  }
  $effect(() => {
    const value = presentation;
    try {
      localStorage.setItem(PRESENTATION_KEY, value);
    } catch {
      // Private windows and blocked storage keep the view for this visit.
    }
  });
  // Nothing chosen leaves nothing to configure.
  $effect(() => {
    if (stage === "configure" && !single && chosen.length === 0)
      onstagechange("pick");
  });

  onMount(() => {
    void creation.experience.load();
    // A link that hands over setup answers opens that use case's card.
    const handed = Object.keys(config.useCaseSetupAnswers ?? {});
    const target = available.find((card) => card.id === handed[0]);
    if (handed.length === 1 && target?.question && !single) {
      // The app is chosen in the service picker, not in the setup.
      const app = config.useCaseSetupAnswers?.[target.id]?.backend?.[0];
      if (
        app &&
        (reviewMode ||
          supportsCreationChoice(runtimeCatalog.get(target.id), "backend", app))
      )
        onSettingChange(target.question.configKey, "backend", app);
      onGoalChange(target.question.configKey, true);
      initial = target.id;
      onstagechange("configure");
    }
  });

  async function addUseCase() {
    onstagechange("pick");
    await tick();
    root?.scrollIntoView({ block: "start", behavior: "smooth" });
  }
</script>

<div
  bind:this={root}
  class="goals-preview"
  data-testid={reviewMode ? "creation-goals-preview" : "creation-goals"}
  data-stage={single ? "configure" : stage}
>
  {#if reviewMode}<div class="preview-caption">
      <span class="preview-dot"></span><span>{tr("wizard.preview.label")}</span>
    </div>{/if}

  {#if single || stage === "configure"}
    <CreationGoalsConfigure
      cards={chosen}
      bind:config
      {runtimeCatalog}
      {reviewMode}
      onGoalChange={changeGoal}
      {onSettingChange}
      {hardware}
      {single}
      {initial}
      onadd={addUseCase}
      {ondisclose}
    />
  {:else}
    <CreationGoalsPick
      cards={ordered}
      {config}
      bind:presentation
      {reviewMode}
      onGoalChange={changeGoal}
      ring={profileRing}
    >
      {#snippet templates()}
        <CreationTemplates
          templates={profiles}
          {live}
          {standardStart}
          expansion={STANDARD_STARTS}
          cards={allCards}
          ontoggle={toggleProfile}
        />
      {/snippet}
    </CreationGoalsPick>
  {/if}
</div>

<style>
  .goals-preview {
    display: grid;
    gap: 18px;
    width: 100%;
    /* Uses the window's width up to the wide card; beyond that it stays centered. */
    max-width: 1440px;
    margin-inline: auto;
    min-width: 0;
    scroll-margin-block-start: 16px;
  }
  :global([data-creation-hosted]) .goals-preview {
    gap: 10px;
  }
  .preview-caption {
    display: flex;
    align-items: center;
    gap: 8px;
    padding-bottom: 14px;
    border-bottom: 1px solid var(--border);
    font-size: 11px;
    color: var(--muted-foreground);
  }
  .preview-dot {
    width: 6px;
    height: 6px;
    border-radius: 50%;
    background: var(--primary);
  }
</style>
