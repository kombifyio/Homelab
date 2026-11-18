<!--
  Add use cases to an existing homelab (Unifier Layout Planner, phase L0).

  Reuses the creation wizard on its Goals step only: the server is the
  homelab's own (inherited), nothing else is asked. The wizard shows the
  recommended distribution of the selection over every server of the homelab;
  pressing "Add use cases" submits an extend run that accepts it, on the
  deployment the recommendation names (this one or another of the homelab). If the homelab changed since the distribution
  was shown, the run stops, nothing is changed, and the updated recommendation
  is shown to be confirmed again.
-->
<script lang="ts">
  import { onMount, untrack } from "svelte";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import { CreationPageFrame } from "#lib/components/wizard/index.js";
  import { getKitDeployment } from "#lib/api/stacks.js";
  import { getHomelab } from "#lib/api/homelab.js";
  import { CANONICAL_USE_CASE_GOALS } from "#lib/wizard/standardBundle.js";
  import { useCaseTitle } from "#lib/wizard/creationGoalText.js";
  import { parseApiError } from "#lib/api/errors.js";
  import type {
    LayoutRecommendationAcceptance,
    LayoutRunTarget,
  } from "#lib/api/layout.js";
  import { AddUseCasesRun } from "#lib/creation/add-use-cases.svelte.js";
  import { clearWizardDraft } from "#lib/wizard/draft-storage.js";
  import { createExpansionConfig, type StackConfig } from "#lib/wizard/index.js";
  import { tr } from "#lib/i18n.svelte.js";
  import UseCasesAddedResult from "./UseCasesAddedResult.svelte";

  interface Props {
    stackId: string;
    /** Use cases to start with, from a deep link; canonical ids only. */
    initialGoals?: string[];
    /** Preserve the guided answers handed over with the selected use case. */
    initialConfig?: StackConfig;
    /** Remove page-owned chrome and spacing when mounted by a host. */
    embedded?: boolean;
  }

  let {
    stackId,
    embedded = false,
    initialGoals = [],
    initialConfig,
  }: Props = $props();

  const run = new AddUseCasesRun();
  // The same draft in the Cloud popup and on this page; an accepted run ends it.
  const draftKey = $derived(`add-use-case-${stackId}`);
  $effect(() => {
    if (run.result) void clearWizardDraft(untrack(() => draftKey));
  });

  // The deep link seeds the first selection only; later edits are the owner's.
  let config = $state<StackConfig>(
    untrack(() => {
      const expansion = createExpansionConfig(initialGoals);
      return initialConfig
        ? {
            ...expansion,
            goals: initialConfig.goals,
            useCaseSetupAnswers: initialConfig.useCaseSetupAnswers,
            useCaseSettings: initialConfig.useCaseSettings,
            useCaseCapabilities: initialConfig.useCaseCapabilities,
            useCaseAccess: initialConfig.useCaseAccess,
          }
        : expansion;
    }),
  );
  let homelabName = $state("");
  let deferredGoals = $state<string[]>([]);
  let loading = $state(true);
  let loadError = $state<string | null>(null);

  onMount(() => {
    void load();
  });

  async function load() {
    loading = true;
    loadError = null;
    try {
      const deployment = await getKitDeployment(stackId);
      homelabName = deployment?.name ?? "";
      // Profile wishes are non-executable. The owner starts one explicitly;
      // the ordinary wizard still checks catalog, placement and admission.
      const home = await getHomelab().catch(() => null);
      const wizard = home?.homelab?.intent.wizard;
      if (wizard && typeof wizard === "object" && "profile_expansions" in wizard) {
        const profiles = (wizard as { profile_expansions?: unknown }).profile_expansions;
        if (Array.isArray(profiles)) {
          deferredGoals = [...new Set(profiles.flatMap((profile) =>
            Array.isArray(profile?.deferred_goals)
              ? profile.deferred_goals.filter((goal: unknown): goal is string => typeof goal === "string")
              : [],
          ))];
        }
      }
    } catch (err) {
      loadError =
        parseApiError(err).message ||
        tr("ui.managedCreationFlow.stackkitDeploymentNotFound");
    } finally {
      loading = false;
    }
  }

  function addUseCases(
    wizardConfig: StackConfig,
    extras?: {
      layoutRecommendation?: LayoutRecommendationAcceptance;
      layoutTarget?: LayoutRunTarget;
    },
  ) {
    void run.submit(
      { stackId, stackName: homelabName },
      wizardConfig,
      extras?.layoutRecommendation,
      extras?.layoutTarget,
    );
  }

  const extended = $derived(
    run.target ?? { stackId, stackName: homelabName },
  );
</script>

<div
  class={embedded ? "w-full p-6" : "mx-auto max-w-6xl p-6 md:p-8"}
  data-testid="add-use-cases-page"
  data-presentation={embedded ? "embedded" : "page"}
>
  {#if loading}
    <LoadingNote label={tr("common.loading")} />
  {:else if loadError}
    <ErrorCallout message={loadError} testId="add-use-cases-error" />
  {:else if run.result}
    <UseCasesAddedResult
      result={run.result}
      stackId={extended.stackId}
      progressHref={run.progressHref(extended.stackId, extended.stackName)}
      {embedded}
    />
  {:else}
    <header class="mb-6 space-y-1">
      <h1 class="text-2xl font-semibold tracking-tight text-foreground">
        {tr("layout.addUseCases.title")}
      </h1>
      <p class="max-w-prose text-sm text-muted-foreground">
        {tr("layout.addUseCases.subtitle")}
      </p>
    </header>
    {#if deferredGoals.length > 0}
      <section class="mb-6 rounded-lg border border-border p-4" aria-label={tr("layout.expansion.title")}>
        <h2 class="font-semibold">{tr("layout.expansion.title")}</h2>
        <p class="mt-1 text-sm text-muted-foreground">{tr("layout.expansion.body")}</p>
        <ul class="mt-3 flex flex-wrap gap-3">
          {#each deferredGoals as goal (goal)}
            <li>
              {#if (CANONICAL_USE_CASE_GOALS as readonly string[]).includes(goal)}
                <a class="text-sm underline" href={`/stacks/${encodeURIComponent(stackId)}/use-cases/new?goals=${encodeURIComponent(goal)}`}>{useCaseTitle(goal)}</a>
              {:else}
                <span class="text-sm text-muted-foreground">{useCaseTitle(goal)} · {tr("layout.expansion.pending")}</span>
              {/if}
            </li>
          {/each}
        </ul>
        <a class="mt-3 inline-block text-sm underline" href={`/stacks/${encodeURIComponent(stackId)}/servers/new`}>{tr("layout.expansion.server")}</a>
      </section>
    {/if}
    <CreationPageFrame
      process={run.process}
      initialConfig={config}
      oncreate={addUseCases}
      isDeploying={run.submitting}
      {draftKey}
      submitLabel={tr("layout.addUseCases.submit")}
      submittingLabel={tr("layout.addUseCases.submitting")}
      layoutStackId={stackId}
      layoutReplacement={run.replacement}
      layoutRefresh={run.refresh}
    >
      {#snippet footerNotice()}
        {#if run.notice}
          <ErrorCallout
            tone={run.notice.tone}
            title={run.notice.title}
            message={run.notice.body}
            details={run.notice.details ?? null}
            testId="add-use-cases-notice"
          />
        {/if}
      {/snippet}
    </CreationPageFrame>
  {/if}
</div>
