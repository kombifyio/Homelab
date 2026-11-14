<script lang="ts">
  import { onMount, tick, untrack } from "svelte";
  import { page } from "$app/state";
  import { replaceState } from "$app/navigation";
  import UseCasePlacement from "#lib/components/hub/UseCasePlacement.svelte";
  import ConfigurationFlow from "#lib/components/hub/ConfigurationFlow.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import {
    BUILDER_GOALS,
    BUILDER_PLACEMENTS,
    readBuilderPlan,
    builderWizardConfig,
  } from "#lib/wizard/builder-handoff.js";
  import { loadWizardDraft, saveWizardDraft } from "#lib/wizard/draft-storage.js";
  import {
    creationUseCaseHandoffKey,
    creationUseCaseWizardConfig,
    readSelfDisclosureHandoff,
    withoutSelfDisclosureParams,
  } from "#lib/wizard/self-disclosure-handoff.js";

  const builderParameter = untrack(() =>
    page.url.searchParams.get("builder-plan"),
  );
  const mixedHandoff = untrack(() =>
    [
      "use-case",
      "experience",
      "goals",
      "placement",
      "hardware",
      "household",
      "motivations",
      "intent",
    ].some((key) => page.url.searchParams.has(key)),
  );
  let builderChoices = $state(
    mixedHandoff ? null : readBuilderPlan(builderParameter),
  );
  const sessionParameter = untrack(() =>
    page.url.searchParams.get("builder-session"),
  );
  let builderSession = $state(
    sessionParameter &&
      /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(
        sessionParameter,
      )
      ? sessionParameter
      : null,
  );
  let builderStarted = $state(false);
  onMount(() => {
    if (!builderChoices || !builderSession) return;
    const session = builderSession;
    void loadWizardDraft(`stack-builder-${session}`).then((draft) => {
      if (draft && builderSession === session) builderStarted = true;
    });
  });
  const selectedBuilderChoices = $derived(
    builderChoices?.filter((choice) => choice.included) ?? [],
  );

  async function startBuilderWizard() {
    const session = crypto.randomUUID();
    // The wizard reads this draft once it opens; the read waits for the write.
    void saveWizardDraft(`stack-builder-${session}`, "stack", {
      step: "goals",
      furthest: "goals",
      config: builderWizardConfig(selectedBuilderChoices),
    });
    const url = new URL((page.shallow?.url ?? page.url).href);
    url.searchParams.set(
      "builder-plan",
      selectedBuilderChoices
        .map(({ idea, placement }) => `${idea}:${placement}`)
        .join(","),
    );
    url.searchParams.set("builder-session", session);
    replaceState(`${url.pathname}${url.search}${url.hash}`, page.state);
    builderSession = session;
    builderStarted = true;
    await tick();
    document.getElementById("builder-plan-summary")?.focus();
  }

  // Answers handed over from the kombify Cloud capture surface. Read once:
  // after they are stored, the URL drops them so a reload does not resend.
  let selfDisclosure = $state(readSelfDisclosureHandoff(page.url.searchParams));
  // A link with answers keeps the creation flow, also once they are stored.
  const handedOver = untrack(() => Boolean(selfDisclosure));
  const creationUseCase = $derived(
    creationUseCaseHandoffKey(page.url.searchParams),
  );
  const initialConfig = $derived(
    creationUseCaseWizardConfig(page.url.searchParams),
  );

  function clearHandoffParams() {
    selfDisclosure = null;
    // The wizard keeps its target and step in shallow history entries.
    replaceState(
      withoutSelfDisclosureParams(page.shallow?.url ?? page.url),
      page.state,
    );
  }
</script>

<svelte:head>
  <title>{tr("wizard.title")} | kombify-Techstack</title>
</svelte:head>

{#if builderParameter !== null && !builderChoices}
  <section class="mx-auto max-w-6xl p-6" role="alert">
    <p>{tr("builderHandoff.invalid")}</p>
    <a href="/stacks/new">{tr("builderHandoff.startEmpty")}</a>
  </section>
{:else if builderChoices && !builderStarted}
  <section class="mx-auto max-w-6xl p-6" aria-labelledby="builder-plan-title">
    <h1 id="builder-plan-title">{tr("builderHandoff.title")}</h1>
    <p>{tr("builderHandoff.description")}</p>
    <ul class="my-6 grid gap-3">
      {#each builderChoices as choice (choice.idea)}
        <li class="rounded-lg border p-4">
          <label class="flex items-center gap-2">
            <input type="checkbox" bind:checked={choice.included} />
            <strong>{tr(`builderHandoff.idea.${choice.idea}`)}</strong>
          </label>
          {#if choice.included}
            <label class="mt-2 block">
              {tr("builderHandoff.placement")}
              <select
                bind:value={choice.placement}
                aria-label={tr("builderHandoff.placementFor", {
                  idea: tr(`builderHandoff.idea.${choice.idea}`),
                })}
                aria-describedby={BUILDER_GOALS[choice.idea] === null
                  ? `builder-note-${choice.idea}`
                  : undefined}
                class="ml-2 rounded border p-2"
              >
                {#each BUILDER_PLACEMENTS as placement}
                  <option value={placement}
                    >{tr(`builderHandoff.placement.${placement}`)}</option
                  >
                {/each}
              </select>
            </label>
            {#if BUILDER_GOALS[choice.idea] === null}
              <p id={`builder-note-${choice.idea}`}>
                {tr("builderHandoff.unsupported")}
              </p>
            {/if}
          {/if}
        </li>
      {/each}
    </ul>
    <p>{tr("builderHandoff.placementNotice")}</p>
    <button
      class="mt-4 rounded bg-primary px-4 py-2 text-primary-foreground"
      disabled={!selectedBuilderChoices.length}
      onclick={startBuilderWizard}>{tr("builderHandoff.openWizard")}</button
    >
  </section>
{:else}
  {#if builderStarted && builderChoices}
    <aside
      id="builder-plan-summary"
      tabindex="-1"
      class="mx-auto max-w-6xl rounded-lg border p-4"
      aria-label={tr("builderHandoff.summaryTitle")}
    >
      <strong>{tr("builderHandoff.summaryTitle")}</strong>
      <p>{tr("builderHandoff.placementNotice")}</p>
      <ul>
        {#each selectedBuilderChoices as choice (choice.idea)}
          <li>
            {tr(`builderHandoff.idea.${choice.idea}`)} — {tr(
              `builderHandoff.placement.${choice.placement}`,
            )}{BUILDER_GOALS[choice.idea] === null
              ? ` (${tr("builderHandoff.unsupported")})`
              : ""}
          </li>
        {/each}
      </ul>
    </aside>
  {/if}
  {#key creationUseCase}
    {#if builderStarted && builderChoices}
      <UseCasePlacement
        initialConfig={builderWizardConfig(builderChoices)}
        draftScope={`stack-builder-${builderSession}`}
      />
    {:else if creationUseCase && initialConfig}
      <!-- A link for one use case (the dashboard, Cloud's first setup): the
           server comes first, then that use case alone. -->
      <UseCasePlacement
        {initialConfig}
        focusGoal={creationUseCase}
        {selfDisclosure}
        onSelfDisclosureSettled={clearHandoffParams}
      />
    {:else if handedOver}
      <ConfigurationFlow
        {initialConfig}
        {selfDisclosure}
        onSelfDisclosureSettled={clearHandoffParams}
      />
    {:else}
      <!-- The owner's existing servers are offered on the Server step. -->
      <UseCasePlacement />
    {/if}
  {/key}
{/if}
