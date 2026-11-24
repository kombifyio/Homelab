<script lang="ts">
  import CreationGoals from "../CreationGoals.svelte";
  import LayoutRecommendationPanel from "../LayoutRecommendationPanel.svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import { onMount } from "svelte";
  import { Check } from "@lucide/svelte";
  import { SegmentedControl, Toggle } from "@kombiverselabs/ui/primitives";
  import { useCaseTitle } from "#lib/wizard/creationGoalText.js";
  import type { HomelabAccessFacts } from "#lib/creation/use-case-access.js";
  import type { UseCaseAccessChoice } from "#lib/wizard/types.js";
  import { useCreationContext } from "#lib/creation/context.js";
  import type { GoalsSlice } from "#lib/creation/core/index.js";
  import {
    acceleratorFromGpu,
    type SetupHardwareHints,
  } from "@kombiverselabs/ui/usecase";
  import type { UseCaseCatalogView } from "#lib/api/useCaseCatalog.js";
  import type { CreationStage } from "#lib/wizard/creationGoals.js";
  import type { WizardPreviewState } from "#lib/wizard/WizardPreviewController.js";
  import type { LayoutPreviewState } from "#lib/wizard/LayoutPreviewController.js";
  import type {
    BundleQuestionDefinition,
    GoalConfigKey,
  } from "#lib/wizard/index.js";

  let {
    config = $bindable(),
    primaryGoalQuestions,
    advancedGoalQuestions,
    onGoalChange,
    onSettingChange,
    recommendationState = { phase: "idle" },
    useCaseCatalog = new Map(),
    catalogState = "ready",
    onCatalogReload,
    layoutState = { phase: "idle" },
    onLayoutRetry,
    layoutPins = {},
    onLayoutPin,
    onLayoutPinUndo,
    onLayoutPinsClear,
    ondisclose,
    focusGoal,
    goalsStage = "pick",
    onGoalsStage,
    asksUseCaseAccess = false,
  }: {
    config: GoalsSlice;
    primaryGoalQuestions: BundleQuestionDefinition<GoalConfigKey>[];
    advancedGoalQuestions: BundleQuestionDefinition<GoalConfigKey>[];
    onGoalChange: (goal: GoalConfigKey, value: boolean) => void;
    onSettingChange: (
      goal: GoalConfigKey,
      id: string,
      value: string | boolean,
    ) => void;
    recommendationState?: WizardPreviewState;
    useCaseCatalog?: Map<string, UseCaseCatalogView>;
    /**
     * Where the release catalog stands. Cards need it: without it every use
     * case would read "Coming soon", which is a lie when the catalog merely
     * failed to load.
     */
    catalogState?: "loading" | "ready" | "error";
    onCatalogReload?: () => void;
    /** The recommended distribution of the selection (existing deployments only). */
    layoutState?: LayoutPreviewState;
    onLayoutRetry?: () => void;
    /** The owner's server per use case; re-solved and re-verified on change. */
    layoutPins?: Record<string, string>;
    onLayoutPin?: (goal: string, serverId: string | null) => void;
    onLayoutPinUndo?: () => void;
    onLayoutPinsClear?: () => void;
    ondisclose?: (event: {
      kind: "detail-level" | "drawer" | "group";
      id?: string;
      open: boolean;
    }) => void;
    /** The one use case the person came for; the step shows only it. */
    focusGoal?: string;
    /** The choice or the configuration; the wizard frame owns it. */
    goalsStage?: CreationStage;
    onGoalsStage: (stage: CreationStage) => void;
    /** The focused use case decides its own reach and people. */
    asksUseCaseAccess?: boolean;
  } = $props();

  const questions = $derived([
    ...primaryGoalQuestions,
    ...advancedGoalQuestions,
  ]);
  const catalog = $derived(useCaseCatalog);

  // The strongest approved server (a GPU first, then memory) drives the
  // guided setup's hardware recommendations. Without servers or access the
  // catalog defaults stay.
  let hardware = $state<SetupHardwareHints>({});
  const creation = useCreationContext();
  onMount(() => {
    creation.inventory
      .listWorkers()
      .then((workers) => {
        const best = workers
          .filter((worker) => worker.approved)
          .sort(
            (a, b) =>
              Number(Boolean(acceleratorFromGpu(b.gpu))) -
                Number(Boolean(acceleratorFromGpu(a.gpu))) ||
              (b.ram_mb ?? 0) - (a.ram_mb ?? 0),
          )[0];
        if (best)
          hardware = {
            gpu: acceleratorFromGpu(best.gpu) ? best.gpu : undefined,
            ramMb: best.ram_mb || undefined,
            server: best.hostname || undefined,
          };
      })
      .catch(() => {});
  });

  // -- reach and people of the focused use case ------------------------------
  // The homelab default is shown as the preselected choice; a different
  // choice is this use case's own decision, and going back to the default
  // withdraws a saved one explicitly.
  const accessGoal = $derived(
    asksUseCaseAccess && focusGoal && config.goals?.[focusGoal as GoalConfigKey]
      ? focusGoal
      : "",
  );
  let accessFacts = $state<HomelabAccessFacts | null>(null);
  let accessRequested = false;
  $effect(() => {
    if (!accessGoal || accessRequested) return;
    accessRequested = true;
    const goal = accessGoal;
    creation
      .homelabAccess()
      .catch((): HomelabAccessFacts => ({ saved: {}, people: [] }))
      .then((facts) => {
        accessFacts = facts;
        // Coming back: the saved decision is the starting point.
        const saved = facts.saved[goal];
        if (saved && !config.useCaseAccess?.[goal])
          config.useCaseAccess = { ...config.useCaseAccess, [goal]: saved };
      });
  });
  const ownAccess = $derived.by(() => {
    const choice = accessGoal ? config.useCaseAccess?.[accessGoal] : undefined;
    return choice && choice.mode !== "inherit" ? choice : undefined;
  });
  const shownAccess = $derived<Partial<UseCaseAccessChoice>>(
    ownAccess ?? {
      mode: accessFacts?.mode,
      audience: accessFacts?.audience,
    },
  );
  const accessPeople = $derived.by(() => {
    const known = accessFacts?.people ?? [];
    const unknown = (ownAccess?.person_refs ?? [])
      .filter((ref) => !known.some((person) => person.ref === ref))
      .map((ref) => ({ ref, name: ref }));
    return [...known, ...unknown];
  });

  function writeAccess(choice: UseCaseAccessChoice | undefined) {
    const next = { ...config.useCaseAccess };
    if (choice) next[accessGoal] = choice;
    else delete next[accessGoal];
    config.useCaseAccess = next;
  }
  function inheritAccess() {
    writeAccess(
      accessFacts?.saved[accessGoal] ? { mode: "inherit" } : undefined,
    );
  }
  function changeAccess(change: Partial<UseCaseAccessChoice>) {
    const next = { ...shownAccess, ...change };
    const mode = next.mode === "remote-private" ? "remote-private" : "local";
    const audience = next.audience ?? "owner";
    if (
      audience !== "selected" &&
      mode === accessFacts?.mode &&
      audience === accessFacts?.audience
    ) {
      inheritAccess();
      return;
    }
    writeAccess({
      mode,
      audience,
      ...(audience === "selected"
        ? { person_refs: [...(next.person_refs ?? [])] }
        : {}),
    });
  }
  function togglePerson(ref: string, chosen: boolean) {
    const refs = (shownAccess.person_refs ?? []).filter(
      (entry) => entry !== ref,
    );
    changeAccess({ person_refs: chosen ? [...refs, ref] : refs });
  }
</script>

<div class="goals-step space-y-8" data-testid="easy-step-1">
  {#if catalogState !== "ready"}
    <header class="space-y-2">
      <p class="text-xs font-semibold uppercase tracking-wider text-primary">
        {tr("wizard.preview.eyebrow")}
      </p>
      <h2 class="text-3xl font-semibold tracking-tight text-foreground">
        {tr("wizard.preview.title")}
      </h2>
      <p class="max-w-prose text-sm text-muted-foreground">
        {tr("wizard.preview.subtitle")}
      </p>
    </header>
  {/if}
  {#if catalogState === "loading"}
    <div
      class="space-y-4"
      aria-busy="true"
      data-testid="use-case-catalog-loading"
    >
      <LoadingNote label={tr("wizard.catalog.loading")} />
      <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3" aria-hidden="true">
        {#each Array(6) as _, index (index)}
          <div
            class="h-40 animate-pulse rounded-2xl border border-border bg-muted/40 motion-reduce:animate-none"
          ></div>
        {/each}
      </div>
    </div>
  {:else if catalogState === "error"}
    <ErrorCallout
      title={tr("wizard.catalog.errorTitle")}
      message={tr("wizard.catalog.errorBody")}
      onRetry={onCatalogReload}
      retryLabel={tr("wizard.catalog.reload")}
      testId="use-case-catalog-error"
    />
  {:else}
    <CreationGoals
      bind:config
      {questions}
      {catalog}
      runtimeCatalog={useCaseCatalog}
      {onGoalChange}
      {onSettingChange}
      {ondisclose}
      smartHomeRecommendation={recommendationState.phase === "resolved"
        ? recommendationState.result.smart_home_recommendation
        : undefined}
      {hardware}
      focus={focusGoal}
      stage={goalsStage}
      onstagechange={onGoalsStage}
    />
    {#if accessGoal && accessFacts}
      {@const useCase = useCaseTitle(accessGoal)}
      <section
        class="use-case-access"
        aria-labelledby="use-case-access-title"
        data-testid="use-case-access"
        data-inherited={ownAccess ? undefined : "true"}
      >
        <header>
          <h3 id="use-case-access-title">
            {tr("layout.addUseCases.access.title", { useCase })}
          </h3>
          {#if ownAccess}
            <button
              type="button"
              class="access-reset"
              data-testid="use-case-access-inherit"
              onclick={inheritAccess}
              >{tr("layout.addUseCases.access.inherit")}</button
            >
          {:else}
            <span class="access-inherited"
              >{tr("layout.addUseCases.access.inherited")}</span
            >
          {/if}
        </header>
        <div class="access-row">
          <span class="access-label"
            >{tr("layout.addUseCases.access.reach")}</span
          >
          <SegmentedControl
            class="access-choice"
            label={tr("layout.addUseCases.access.reach")}
            value={shownAccess.mode ?? ""}
            items={[
              {
                value: "local",
                label: tr("layout.addUseCases.access.reach.local"),
              },
              {
                value: "remote-private",
                label: tr("layout.addUseCases.access.reach.remote"),
              },
            ]}
            onchange={(mode) =>
              changeAccess({ mode: mode as UseCaseAccessChoice["mode"] })}
          />
        </div>
        <div class="access-row">
          <span class="access-label"
            >{tr("layout.addUseCases.access.people")}</span
          >
          <SegmentedControl
            class="access-choice"
            label={tr("layout.addUseCases.access.people")}
            value={shownAccess.audience ?? ""}
            items={[
              {
                value: "owner",
                label: tr("layout.addUseCases.access.people.owner"),
              },
              {
                value: "household",
                label: tr("layout.addUseCases.access.people.household"),
              },
              {
                value: "selected",
                label: tr("layout.addUseCases.access.people.selected"),
                disabled: accessPeople.length === 0,
              },
            ]}
            onchange={(audience) =>
              changeAccess({
                audience: audience as UseCaseAccessChoice["audience"],
              })}
          />
        </div>
        {#if shownAccess.audience === "selected"}
          <div
            class="access-people"
            role="group"
            aria-label={tr("layout.addUseCases.access.chosen", { useCase })}
            data-testid="use-case-access-people"
          >
            {#each accessPeople as person (person.ref)}
              {@const chosen = (shownAccess.person_refs ?? []).includes(
                person.ref,
              )}
              <Toggle
                pressed={chosen}
                label={person.name}
                onchange={(next) => togglePerson(person.ref, next)}
                >{#if chosen}<Check
                    size={14}
                    aria-hidden="true"
                  />{/if}{person.name}</Toggle
              >
            {/each}
          </div>
        {/if}
      </section>
    {/if}
  {/if}
  {#if layoutState.phase !== "idle"}
    <LayoutRecommendationPanel
      state={layoutState}
      onretry={onLayoutRetry}
      pins={layoutPins}
      onpin={onLayoutPin}
      onundopin={onLayoutPinUndo}
      onclearpins={onLayoutPinsClear}
    />
  {/if}
</div>

<style>
  /* Hosted, the step shares one screen with the bar above it. */
  :global([data-creation-hosted]) .goals-step > :global(* + *) {
    margin-top: 12px;
  }
  .use-case-access {
    display: grid;
    gap: 14px;
    padding-top: 20px;
    border-top: 1px solid var(--border);
  }
  .use-case-access header {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: space-between;
    gap: 8px 16px;
  }
  .use-case-access h3 {
    font-size: 15px;
    font-weight: 620;
  }
  .access-inherited {
    padding: 4px 9px;
    border: 1px solid var(--border);
    border-radius: 999px;
    color: var(--muted-foreground);
    font-size: 11px;
  }
  .access-reset {
    color: var(--primary);
    font-size: 12px;
    text-decoration: underline;
    text-underline-offset: 3px;
    cursor: pointer;
  }
  .access-reset:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 3px;
  }
  .access-row {
    display: grid;
    grid-template-columns: 120px minmax(0, 1fr);
    align-items: center;
    gap: 6px 16px;
  }
  .access-label {
    color: var(--muted-foreground);
    font-size: 12px;
  }
  .access-row :global(.access-choice) {
    flex-wrap: wrap;
  }
  .access-people {
    display: flex;
    flex-wrap: wrap;
    gap: 8px;
    padding-left: 136px;
  }
  @media (max-width: 560px) {
    .access-row {
      grid-template-columns: minmax(0, 1fr);
    }
    .access-people {
      padding-left: 0;
    }
  }
</style>
