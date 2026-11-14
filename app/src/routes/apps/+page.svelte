<script lang="ts">
  /**
   * Applications: everything a user opens through a UI on their servers
   * (Immich, Hermes, a kombify tool), as compact cards or logos only. The
   * Services page keeps every service, system services included, with the
   * larger cards and their settings.
   */
  import { onMount } from "svelte";
  import { goto } from "$app/navigation";
  import { PageHeader } from "@kombiverselabs/ui/shell";
  import { SegmentedControl } from "@kombiverselabs/ui/primitives";
  import { EntityActionMenu } from "@kombiverselabs/ui/entity";
  import { UseCaseCardCompact } from "@kombiverselabs/ui/usecase";
  import { Plus, SlidersHorizontal, Trash2 } from "@lucide/svelte";
  import Button from "#lib/components/ui/Button.svelte";
  import { theme } from "#lib/stores/theme.js";
  import { listKitDeployments } from "#lib/api/stacks.js";
  import {
    getUseCaseConfiguration,
    type InstalledUseCaseConfiguration,
  } from "#lib/api/useCaseConfiguration.js";
  import {
    addUseCasesHref,
    configureUseCasesHref,
    placeUseCasesHref,
    removeUseCasesHref,
  } from "#lib/server-links.js";
  import ErrorCallout from "#lib/components/ui/ErrorCallout.svelte";
  import TrademarkNotice from "#lib/components/TrademarkNotice.svelte";
  import LoadingNote from "#lib/components/ui/LoadingNote.svelte";
  import { tr } from "#lib/i18n.svelte.js";
  import {
    listCanonicalServices,
    type CanonicalService,
  } from "#lib/api/services.js";
  import ApplicationCard from "#lib/components/dashboard/ApplicationCard.svelte";
  import ApplicationDetailSheet from "#lib/components/dashboard/ApplicationDetailSheet.svelte";
  import { createAppQuickActions } from "#lib/services/app-quick-actions.svelte.js";
  import {
    foldApplications,
    serviceDisplayName,
  } from "#lib/dashboard/homelab-model.js";

  type View = "logos" | "list" | "cards";
  const VIEW_KEY = "techstack.apps.view";

  let services = $state<CanonicalService[]>([]);
  let loading = $state(true);
  let failed = $state(false);
  let retrying = $state(false);
  let view = $state<View>("list");

  /** One entry per application and server; the same app on two servers is two. */
  const applications = $derived.by(() => {
    const byServer: Record<string, CanonicalService[]> = {};
    for (const service of services) {
      const key = service.server_id || `workload:${service.id}`;
      (byServer[key] ??= []).push(service);
    }
    const entries: { service: CanonicalService; members: CanonicalService[] }[] =
      [];
    for (const placed of Object.values(byServer)) {
      const { apps, members } = foldApplications(placed);
      for (const app of apps)
        entries.push({ service: app, members: members[app.id] ?? [app] });
    }
    return entries.sort((left, right) =>
      serviceDisplayName(left.service).localeCompare(
        serviceDisplayName(right.service),
      ),
    );
  });

  const { provider: quick, start: startQuickActions } = createAppQuickActions({
    refresh: async () => {
      try {
        services = await listCanonicalServices();
      } catch {
        // Keep the last list; the next action or reload tries again.
      }
    },
    services: () => services,
  });
  onMount(startQuickActions);

  function setView(next: View) {
    view = next;
    try {
      localStorage.setItem(VIEW_KEY, next);
    } catch {
      // Storage is a convenience; the view still switches.
    }
  }

  /** M and L cards open the detail sheet; S goes straight to the app. */
  let sheetServiceId = $state<string | null>(null);
  const sheetEntry = $derived(
    applications.find((entry) => entry.service.id === sheetServiceId),
  );

  function openDetails(serviceId: string) {
    void goto(`/services?service=${encodeURIComponent(serviceId)}`);
  }

  async function loadApplications() {
    failed = false;
    try {
      services = await listCanonicalServices();
    } catch {
      failed = true;
    } finally {
      loading = false;
      retrying = false;
    }
  }

  function retryApplications() {
    retrying = true;
    void loadApplications();
  }

  /*
   * Installed use cases, one shared use-case row each, with their changes
   * (configure, move, remove) in the row's action menu. A deployment whose
   * configuration cannot be read is left out; the apps above stay readable.
   */
  type InstalledUseCase = {
    stackId: string;
    stackName: string;
    useCase: InstalledUseCaseConfiguration;
  };
  let useCases = $state<InstalledUseCase[]>([]);
  let stackIds = $state<string[]>([]);
  const multipleStacks = $derived(stackIds.length > 1);
  /** One StackKit goes straight to its Goals step; several pick a place first. */
  const addUseCasesTarget = $derived(
    stackIds.length === 1 && stackIds[0] ? addUseCasesHref(stackIds[0]) : placeUseCasesHref(),
  );
  const resolvedAppearance = theme.resolved;

  async function loadUseCases(signal: AbortSignal) {
    try {
      const stacks = await listKitDeployments();
      const results = await Promise.allSettled(
        stacks.map(async (stack) => ({
          stack,
          view: await getUseCaseConfiguration(stack.id, signal),
        })),
      );
      if (signal.aborted) return;
      stackIds = stacks.map((stack) => stack.id);
      useCases = results.flatMap((result) =>
        result.status === "fulfilled"
          ? result.value.view.use_cases.map((useCase) => ({
              stackId: result.value.stack.id,
              stackName: result.value.stack.name,
              useCase,
            }))
          : [],
      );
    } catch {
      // Without a readable deployment list the section stays hidden.
    }
  }

  function useCaseHref(entry: InstalledUseCase, base: string): string {
    const separator = base.includes("?") ? "&" : "?";
    return `${base}${separator}use-case=${encodeURIComponent(entry.useCase.id)}`;
  }

  /** Configure opens on this use case's first configurable workload, if any. */
  function configureHref(entry: InstalledUseCase): string | undefined {
    const workload = entry.useCase.workloads.find((member) => member.eligible);
    return workload
      ? `${configureUseCasesHref(entry.stackId)}?workload=${encodeURIComponent(workload.workload_ref)}`
      : undefined;
  }

  function useCaseActions(entry: InstalledUseCase) {
    const configure = configureHref(entry);
    const removable = entry.useCase.workloads.some(
      (member) => member.removal_eligible,
    );
    return [
      {
        id: "configure",
        label: tr("ui.apps.useCaseConfigure"),
        icon: SlidersHorizontal,
        href: configure,
        state: configure ? ("ready" as const) : ("disabled" as const),
      },
      {
        id: "remove",
        label: tr("ui.apps.useCaseRemove"),
        icon: Trash2,
        href: useCaseHref(entry, removeUseCasesHref(entry.stackId)),
        state: removable ? ("ready" as const) : ("disabled" as const),
      },
    ];
  }

  function useCaseVariant(entry: InstalledUseCase): string {
    const tools = [
      ...new Set(
        entry.useCase.workloads
          .map((member) => member.alternative)
          .filter(Boolean),
      ),
    ].join(" · ");
    return multipleStacks
      ? [entry.stackName, tools].filter(Boolean).join(" · ")
      : tools;
  }

  onMount(() => {
    const useCaseLoad = new AbortController();
    void loadUseCases(useCaseLoad.signal);
    return () => useCaseLoad.abort();
  });

  onMount(() => {
    try {
      const stored = localStorage.getItem(VIEW_KEY);
      if (stored === "logos" || stored === "cards") view = stored;
    } catch {
      // No stored preference.
    }
    void loadApplications();
  });
</script>

<div class="p-4 md:p-6" data-testid="apps-page">
  <PageHeader
    title={tr("nav.apps")}
    description={tr("ui.apps.description")}
  >
    {#snippet actions()}
      <Button
        variant="primary"
        testId="add-use-cases-button"
        onclick={() => goto(addUseCasesTarget)}
      >
        <Plus class="h-4 w-4" />
        {tr("layout.addUseCases.entry")}
      </Button>
      <!-- The same switch as the server list: a view switcher is a group of
           toggle buttons (aria-pressed), not a tablist: it changes how the
           same content is drawn, not which panel is shown. -->
      <div data-testid="apps-view-switch">
        <SegmentedControl
          items={[
            { value: "logos", label: tr("ui.apps.viewLogos") },
            { value: "list", label: tr("ui.apps.viewList") },
            { value: "cards", label: tr("ui.apps.viewCards") },
          ]}
          value={view}
          label={tr("ui.apps.view")}
          onchange={(next) => setView(next as View)}
        />
      </div>
    {/snippet}
  </PageHeader>

  {#if quick.error}
    <ErrorCallout class="mt-3" testId="apps-action-error" message={quick.error} />
  {/if}

  {#if loading}
    <LoadingNote class="mt-4" label={tr("ui.apps.loading")} />
  {:else if failed}
    <ErrorCallout
      class="mt-4"
      testId="apps-unavailable"
      message={tr("ui.apps.unavailable")}
      onRetry={retryApplications}
      {retrying}
    />
  {:else if applications.length === 0}
    <p class="mt-4 text-sm text-muted-foreground" data-testid="apps-empty">
      {tr("ui.apps.empty")}
    </p>
  {:else if view === "logos"}
    <div
      class="mt-4 grid grid-cols-[repeat(auto-fill,minmax(4.5rem,1fr))] gap-3 sm:grid-cols-[repeat(auto-fill,minmax(5.5rem,1fr))]"
      data-testid="apps-logos"
    >
      {#each applications as entry (entry.service.id)}
        <ApplicationCard
          service={entry.service}
          members={entry.members}
          size="s"
          onDetails={() => openDetails(entry.service.id)}
        />
      {/each}
    </div>
  {:else if view === "cards"}
    <div
      class="mt-4 grid grid-cols-1 gap-2 sm:grid-cols-[repeat(auto-fill,minmax(19rem,1fr))]"
      data-testid="apps-cards"
    >
      {#each applications as entry (entry.service.id)}
        <ApplicationCard
          service={entry.service}
          members={entry.members}
          size="l"
          actions={quick.actionsFor(entry.members)}
          onDetails={() => (sheetServiceId = entry.service.id)}
        />
      {/each}
    </div>
  {:else}
    <div
      class="mt-4 grid grid-cols-1 gap-1.5 sm:grid-cols-[repeat(auto-fill,minmax(15rem,1fr))]"
      data-testid="apps-list"
    >
      {#each applications as entry (entry.service.id)}
        <ApplicationCard
          service={entry.service}
          members={entry.members}
          actions={quick.actionsFor(entry.members)}
          onDetails={() => (sheetServiceId = entry.service.id)}
        />
      {/each}
    </div>
  {/if}

  {#if useCases.length > 0}
    <section
      class="mt-8"
      aria-labelledby="installed-use-cases-title"
      data-testid="installed-use-cases"
    >
      <h2
        id="installed-use-cases-title"
        class="mb-3 text-base font-semibold text-foreground"
      >
        {tr("ui.homelab.installedUseCases")}
      </h2>
      <div
        class="grid grid-cols-1 gap-2 sm:grid-cols-[repeat(auto-fill,minmax(19rem,1fr))]"
      >
        {#each useCases as entry (`${entry.stackId}:${entry.useCase.id}`)}
          <UseCaseCardCompact
            data-testid="installed-use-case-row"
            data-stack-id={entry.stackId}
            data-use-case-id={entry.useCase.id}
            name={entry.useCase.name}
            layout="row"
            art={entry.useCase.id}
            dark={$resolvedAppearance === "dark"}
            planState="included"
            selectable={false}
            selected={false}
            description={useCaseVariant(entry)}
            detailsHref={configureHref(entry)}
          >
            {#snippet controls()}
              <EntityActionMenu
                actions={useCaseActions(entry)}
                label={tr("ui.nodeRow.moreActions", { app: entry.useCase.name })}
              />
            {/snippet}
          </UseCaseCardCompact>
        {/each}
      </div>
    </section>
  {/if}

  {#if !loading && !failed && applications.length > 0}
    <TrademarkNotice class="mt-6" />
  {/if}
</div>

{#if sheetEntry}
  <ApplicationDetailSheet
    open
    onclose={() => (sheetServiceId = null)}
    service={sheetEntry.service}
    members={sheetEntry.members}
    actions={quick.actionsFor(sheetEntry.members)}
  />
{/if}
